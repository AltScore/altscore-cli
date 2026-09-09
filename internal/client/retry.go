package client

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	retryAttempts = 3
	retryBase     = 200 * time.Millisecond
	retryCap      = 2 * time.Second
)

var retryDisabled = envDisablesRetry()

func envDisablesRetry() bool {
	switch strings.ToLower(os.Getenv("ALTSCORE_NO_RETRY")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// SetRetryDisabled turns connect-phase retry off process-wide. The CLI calls it
// for --no-retry; ALTSCORE_NO_RETRY=1 does the same without the flag.
func SetRetryDisabled(disabled bool) {
	retryDisabled = disabled
}

// send is the single transport chokepoint: doRequest, doRawOnce and
// Authenticate all go through it, so there is one place that decides what a
// network failure means.
//
// newRequest must build a FRESH request on every call. HTTPClient.Do consumes
// and closes the request body, so an attempt cannot reuse the previous one.
//
// Retry here is connect-phase only, and it composes with the 401 token-refresh
// replay in Do/DoKeepBody/DoRaw without multiplying anything: a 401 is an HTTP
// status, never a transport error, so the predicate below can never fire on it.
// The two mechanisms trigger on disjoint conditions, and the attempt counter is
// per send call, so a refreshed replay starts from attempt 1 again.
func send(hc *http.Client, newRequest func() (*http.Request, error)) (*http.Response, error) {
	attempts := retryAttempts
	if retryDisabled {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := newRequest()
		if err != nil {
			return nil, err
		}

		resp, err := hc.Do(req)
		if err == nil {
			if attempt > 0 {
				fmt.Fprintf(os.Stderr, "# request succeeded after %s\n", retryCount(attempt))
			}
			return resp, nil
		}
		lastErr = err

		if attempt == attempts-1 || !isRetryableConnectError(err) {
			break
		}

		delay := retryDelay(attempt)
		fmt.Fprintf(os.Stderr, "# retry %d/%d: %v (retrying in %v)\n", attempt+1, attempts, retryCause(err), delay)
		time.Sleep(delay)
	}

	return nil, fmt.Errorf("request failed: %w", lastErr)
}

// isRetryableConnectError reports whether err proves the request never reached
// the server, which is the only case where replaying it is safe. The CLI calls
// genuinely non-idempotent endpoints (workflows/import, mapping-tables/import,
// tasks/{alias} minting an immutable version, workflows/{id}/execute, the
// billable requests/sync), so anything ambiguous must not be retried: a dial
// that never connected is the one failure that cannot have had a side effect.
//
// Deliberately NOT retryable: ECONNRESET, io.ErrUnexpectedEOF, response header
// timeouts, and every failure while reading the response body (structurally
// impossible here, since body reads happen after send returns).
func isRetryableConnectError(err error) bool {
	if err == nil {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// net.OpError names the phase it failed in. Only "dial" is provably
	// pre-write; a "read" or "write" op may already have reached the server,
	// so it stops here even when it carries an otherwise retryable errno.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT:
			return true
		}
	}

	return false
}

// retryDelay is full jitter over an exponentially growing window: attempt 0
// waits up to retryBase, attempt 1 up to twice that, capped at retryCap. It
// truncates rather than rounds, so the slept value is the one printed and can
// never round up to the window bound.
func retryDelay(attempt int) time.Duration {
	window := retryBase << attempt
	if window > retryCap || window <= 0 {
		window = retryCap
	}
	return time.Duration(rand.Int64N(int64(window))).Truncate(time.Millisecond)
}

// retryCause strips the url.Error wrapper net/http adds so the retry line shows
// the dial failure instead of repeating the method and URL.
func retryCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

func retryCount(n int) string {
	if n == 1 {
		return "1 retry"
	}
	return fmt.Sprintf("%d retries", n)
}
