package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AltScore/altscore-cli/internal/config"
)

// dialErr is how a real connect failure arrives: an errno inside a "dial"
// net.OpError, inside the url.Error net/http adds, inside the CLI's own wrap.
func dialErr(errno syscall.Errno) error {
	return fmt.Errorf("request failed: %w", &url.Error{
		Op:  "Post",
		URL: "https://bc.altscore.ai/v2/workflows/apply",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errno},
	})
}

// readOpErr is a failure on an established connection: the same errno, but a
// phase that may already have handed the request to the server.
func readOpErr(errno syscall.Errno) error {
	return fmt.Errorf("request failed: %w", &url.Error{
		Op:  "Post",
		URL: "https://bc.altscore.ai/v1/requests/sync",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: errno},
	})
}

func TestIsRetryableConnectError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// --- retryable: provably pre-write ---
		{
			name: "DNS error",
			err:  &net.DNSError{Err: "no such host", Name: "bc.altscore.ai", IsNotFound: true},
			want: true,
		},
		{
			name: "DNS error wrapped by net/http and the client",
			err:  fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: "https://bc.altscore.ai", Err: &net.DNSError{Err: "server misbehaving"}}),
			want: true,
		},
		{
			name: "dial OpError carrying no errno",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: no route to host")},
			want: true,
		},
		{name: "dial ECONNREFUSED", err: dialErr(syscall.ECONNREFUSED), want: true},
		{name: "dial ENETUNREACH", err: dialErr(syscall.ENETUNREACH), want: true},
		{name: "dial EHOSTUNREACH", err: dialErr(syscall.EHOSTUNREACH), want: true},
		{name: "dial ETIMEDOUT", err: dialErr(syscall.ETIMEDOUT), want: true},
		{name: "bare ECONNREFUSED", err: syscall.ECONNREFUSED, want: true},
		{name: "bare ENETUNREACH", err: syscall.ENETUNREACH, want: true},
		{name: "bare EHOSTUNREACH", err: syscall.EHOSTUNREACH, want: true},
		{name: "bare ETIMEDOUT", err: syscall.ETIMEDOUT, want: true},
		{
			name: "retryable errno behind several wraps",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("request failed: %w", syscall.ECONNREFUSED)),
			want: true,
		},

		// --- not retryable: the server may already have acted ---
		{name: "nil", err: nil, want: false},
		{name: "bare ECONNRESET", err: syscall.ECONNRESET, want: false},
		{name: "read ECONNRESET", err: readOpErr(syscall.ECONNRESET), want: false},
		{
			name: "write ECONNRESET",
			err:  &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET},
			want: false,
		},
		{
			// The errno is on the retryable list, but the op is not pre-write,
			// so the phase wins: the request may already have been sent.
			name: "read ETIMEDOUT",
			err:  readOpErr(syscall.ETIMEDOUT),
			want: false,
		},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: false},
		{name: "unexpected EOF wrapped", err: fmt.Errorf("request failed: %w", io.ErrUnexpectedEOF), want: false},
		{name: "EOF", err: io.EOF, want: false},
		{name: "EPIPE", err: syscall.EPIPE, want: false},
		{name: "closed pipe", err: io.ErrClosedPipe, want: false},
		{name: "response body read failure", err: fmt.Errorf("cannot read response: %w", io.ErrUnexpectedEOF), want: false},
		{name: "response header timeout", err: errors.New("net/http: timeout awaiting response headers"), want: false},
		{
			name: "HTTP status error is never a transport error",
			err:  formatHTTPError(http.StatusInternalServerError, []byte(`{"code":"InternalServerError","message":"boom"}`)),
			want: false,
		},
		{name: "HTTP 401", err: formatHTTPError(http.StatusUnauthorized, nil), want: false},
		{name: "opaque error", err: errors.New("something went wrong"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableConnectError(tt.err); got != tt.want {
				t.Fatalf("isRetryableConnectError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestEnvDisablesRetry(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"", false},
		{"0", false},
		{"false", false},
	}

	for _, tt := range tests {
		t.Run("ALTSCORE_NO_RETRY="+tt.value, func(t *testing.T) {
			t.Setenv("ALTSCORE_NO_RETRY", tt.value)
			if got := envDisablesRetry(); got != tt.want {
				t.Fatalf("envDisablesRetry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryDelay_FullJitterWithinWindow(t *testing.T) {
	for attempt := 0; attempt < 6; attempt++ {
		window := retryBase << attempt
		if window > retryCap {
			window = retryCap
		}
		for i := 0; i < 500; i++ {
			d := retryDelay(attempt)
			if d < 0 || d >= window {
				t.Fatalf("attempt %d: delay %v outside [0, %v)", attempt, d, window)
			}
			if d != d.Truncate(time.Millisecond) {
				t.Fatalf("attempt %d: delay %v is not a whole number of ms", attempt, d)
			}
		}
	}
}

// TestSend_RetriesDialFailureThenSucceeds drives send through a real dial
// failure: the first attempt targets a closed port, the second the live server.
func TestSend_RetriesDialFailureThenSucceeds(t *testing.T) {
	enableRetry(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	deadURL := closedPortURL(t)

	var calls int
	resp, err := send(sharedHTTPClient, func() (*http.Request, error) {
		calls++
		target := deadURL
		if calls > 1 {
			target = srv.URL
		}
		return http.NewRequest(http.MethodPost, target, strings.NewReader("body"))
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer resp.Body.Close()

	if calls != 2 {
		t.Fatalf("newRequest called %d times, want 2", calls)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

func TestSend_GivesUpAfterRetryAttempts(t *testing.T) {
	enableRetry(t)

	deadURL := closedPortURL(t)

	var calls int
	_, err := send(sharedHTTPClient, func() (*http.Request, error) {
		calls++
		return http.NewRequest(http.MethodGet, deadURL, nil)
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != retryAttempts {
		t.Fatalf("newRequest called %d times, want %d", calls, retryAttempts)
	}
	if !strings.HasPrefix(err.Error(), "request failed: ") {
		t.Fatalf("error %q lost the \"request failed\" prefix", err)
	}
	if !isRetryableConnectError(err) {
		t.Fatalf("wrapped error %q no longer matches the predicate through errors.As", err)
	}
}

func TestSend_NoRetryDisablesReplay(t *testing.T) {
	enableRetry(t)
	SetRetryDisabled(true)

	deadURL := closedPortURL(t)

	var calls int
	if _, err := send(sharedHTTPClient, func() (*http.Request, error) {
		calls++
		return http.NewRequest(http.MethodGet, deadURL, nil)
	}); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("newRequest called %d times, want 1", calls)
	}
}

// TestSend_DoesNotRetryHTTPStatus is the guard on the non-idempotent endpoints:
// a status, however bad, means the server already acted.
func TestSend_DoesNotRetryHTTPStatus(t *testing.T) {
	enableRetry(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	resp, err := send(sharedHTTPClient, func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer resp.Body.Close()

	if got := hits.Load(); got != 1 {
		t.Fatalf("server hit %d times, want 1: a 5xx must never be replayed", got)
	}
}

func TestSend_ReturnsRequestBuildErrorUnretried(t *testing.T) {
	enableRetry(t)

	want := errors.New("cannot open file: nope")
	var calls int
	_, err := send(sharedHTTPClient, func() (*http.Request, error) {
		calls++
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("newRequest called %d times, want 1", calls)
	}
}

// TestDoRaw_401RetrySendsTheSameBytes is the regression test for the drained
// body. DoRaw replays after refreshing the token, and both attempts must carry
// identical bytes. The factory hands out a one-shot io.Pipe reader, so a DoRaw
// that reused the first reader could not possibly pass.
func TestDoRaw_401RetrySendsTheSameBytes(t *testing.T) {
	enableRetry(t)

	const payload = "the-file-contents"

	log, c := uploadRecorder(t)

	var factoryCalls int
	newBody := func() (io.Reader, error) {
		factoryCalls++
		pr, pw := io.Pipe()
		go func() {
			_, err := io.WriteString(pw, payload)
			pw.CloseWithError(err)
		}()
		return pr, nil
	}

	respBody, status, err := c.DoRaw("POST", "borrower_central", uploadPath, newBody, "multipart/form-data; boundary=fixed")
	if err != nil {
		t.Fatalf("DoRaw: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(respBody) != `{"id":"att-1"}` {
		t.Fatalf("respBody = %q", respBody)
	}

	if got := log.tokenCalls.Load(); got != 1 {
		t.Fatalf("token refreshed %d times, want 1", got)
	}
	if factoryCalls != 2 {
		t.Fatalf("newBody called %d times, want 2 (one per attempt)", factoryCalls)
	}

	uploads := log.snapshot()
	if len(uploads) != 2 {
		t.Fatalf("server saw %d uploads, want 2", len(uploads))
	}
	if uploads[0] != payload {
		t.Fatalf("first attempt sent %q, want %q", uploads[0], payload)
	}
	if uploads[1] != uploads[0] {
		t.Fatalf("retry sent %q, want the same bytes as the first attempt %q", uploads[1], uploads[0])
	}
	if c.Profile.AccessToken != "fresh-token" {
		t.Fatalf("access token = %q, want the refreshed one", c.Profile.AccessToken)
	}
}

// TestDoRaw_SharedReaderBreaksTheRetry pins down the bug the factory fixes, by
// emulating the old io.Reader parameter: one reader handed to both attempts.
// Two shapes of reader, two different failures, neither of them an upload.
func TestDoRaw_SharedReaderBreaksTheRetry(t *testing.T) {
	const payload = "the-file-contents"

	// A pipe the first attempt drained and the transport then closed. The
	// replay fails outright instead of sending an empty body: a loud "closed
	// pipe" error, and the aborted request can still reach the server.
	t.Run("closed pipe fails the replay", func(t *testing.T) {
		enableRetry(t)

		log, c := uploadRecorder(t)

		pr, pw := io.Pipe()
		go func() {
			_, err := io.WriteString(pw, payload)
			pw.CloseWithError(err)
		}()
		shared := func() (io.Reader, error) { return pr, nil }

		_, _, err := c.DoRaw("POST", "borrower_central", uploadPath, shared, "text/plain")
		if err == nil {
			t.Fatal("expected the replay to fail on the drained pipe")
		}
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("err = %v, want io.ErrClosedPipe", err)
		}
		if isRetryableConnectError(err) {
			t.Fatalf("a closed pipe must never count as a connect-phase failure: %v", err)
		}

		// Whether the aborted replay reaches the handler is a race, so only
		// the first upload is asserted on.
		uploads := log.snapshot()
		if len(uploads) == 0 || uploads[0] != payload {
			t.Fatalf("uploads = %q, want the payload first", uploads)
		}
	})

	// A drained bytes.Reader reports EOF rather than an error, which is the
	// silent shape: the server accepts the retry with an empty body.
	t.Run("drained reader uploads nothing", func(t *testing.T) {
		enableRetry(t)

		log, c := uploadRecorder(t)

		r := bytes.NewReader([]byte(payload))
		shared := func() (io.Reader, error) { return r, nil }

		if _, _, err := c.DoRaw("POST", "borrower_central", uploadPath, shared, "text/plain"); err != nil {
			t.Fatalf("DoRaw: %v", err)
		}

		uploads := log.snapshot()
		if len(uploads) != 2 {
			t.Fatalf("server saw %d uploads, want 2", len(uploads))
		}
		if uploads[0] != payload {
			t.Fatalf("first attempt sent %q, want %q", uploads[0], payload)
		}
		if uploads[1] != "" {
			t.Fatalf("retry sent %q, want the empty body a drained reader produces", uploads[1])
		}
	})
}

const uploadPath = "/v1/documents/doc-1/attachments/upload"

// uploadLog records what the fake server received. Guarded because the handler
// runs on the server's goroutine while the test asserts on the test's.
type uploadLog struct {
	mu         sync.Mutex
	uploads    []string
	tokenCalls atomic.Int64
}

func (l *uploadLog) add(body string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.uploads = append(l.uploads, body)
	return len(l.uploads)
}

func (l *uploadLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.uploads...)
}

// uploadRecorder serves the token endpoint, 401s the first upload and accepts
// the second, recording every upload body it saw.
func uploadRecorder(t *testing.T) (*uploadLog, *Client) {
	t.Helper()

	log := &uploadLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			log.tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh-token","token_type":"Bearer"}`))
			return
		}
		if r.URL.Path != uploadPath {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, readErr := io.ReadAll(r.Body)
		record := string(body)
		if readErr != nil {
			// An attempt whose reader was already consumed aborts
			// mid-transfer; record it instead of failing the test, so the
			// caller can assert on what the server actually received.
			record = "<aborted>"
		}

		if log.add(record) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if readErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"att-1"}`))
	}))
	t.Cleanup(srv.Close)

	return log, newRawTestClient(t, srv.URL)
}

// newRawTestClient wires a client to an httptest server for both the API and
// the token endpoint. Profiles is empty on purpose: refreshToken only persists
// when the active profile is present, so this cannot rewrite the real config.
// HOME is redirected as well, in case that ever changes.
func newRawTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{
		DefaultProfile: "test",
		Profiles:       map[string]config.Profile{},
	}
	p := config.Profile{
		Environment:  "staging",
		AccessToken:  "stale-token",
		TenantID:     "tenant",
		ClientID:     "id",
		ClientSecret: "secret",
	}
	c := New(cfg, "test", &p, false)
	c.BaseURLOverrides = map[string]string{
		"borrower_central": baseURL,
		"auth":             baseURL,
	}
	return c
}

// closedPortURL returns a URL whose port is bound and released, so connecting
// to it is refused rather than routed somewhere real.
func closedPortURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + addr
}

// enableRetry forces retry on for the duration of a test, so an
// ALTSCORE_NO_RETRY in the developer's environment cannot mask a failure, and
// restores whatever it was afterwards.
func enableRetry(t *testing.T) {
	t.Helper()
	prev := retryDisabled
	retryDisabled = false
	t.Cleanup(func() { retryDisabled = prev })
}
