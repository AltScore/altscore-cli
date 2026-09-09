package client

import (
	"net"
	"net/http"
	"time"
)

// Phase timeouts for every request the CLI makes. They bound the phases that
// can hang with nothing coming back; they deliberately do not bound body
// transfer.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 60 * time.Second
	maxIdleConnsPerHost   = 8
)

// sharedHTTPClient is the one client every request goes through, so transport
// tuning and the connect-phase retry in send apply uniformly (including the
// token refresh in auth.go).
//
// Timeout is deliberately left at zero. http.Client.Timeout caps the whole
// exchange including body transfer, and the slowest calls here are legitimately
// long: workflows-v2 apply plans, validates, writes and publishes under a lock;
// execute-batch runs 50 executions in parallel; the multipart upload streams a
// file of unbounded size; requests/sync blocks on a third-party bureau. With no
// p99 to size it against, any total number would cut off a working call. The
// bounds live on the transport instead.
var sharedHTTPClient = &http.Client{Transport: newTransport()}

// newTransport clones the stdlib default so connection pooling, proxy handling
// and HTTP/2 stay as configured, then tightens the phase timeouts.
func newTransport() *http.Transport {
	var t *http.Transport
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		t = def.Clone()
	} else {
		t = &http.Transport{}
	}

	// Clone copies DialContext as a closure, so the dial timeout has to be
	// replaced rather than tweaked.
	t.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	t.ResponseHeaderTimeout = responseHeaderTimeout
	// The default of 2 throttles the apply rescope path and the normalizer
	// fan-outs, which make many sequential per-entity calls to one host.
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost

	return t
}
