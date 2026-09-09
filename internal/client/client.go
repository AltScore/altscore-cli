package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/config"
)

// formatHTTPError parses the standard borrower-central error envelope and
// formats it for human consumption. Surfaces details.errorSubCode (which
// callers script against per CLAUDE.md) and any per-field validation
// messages in details. Falls back to the raw body when the envelope shape
// doesn't match (e.g. a non-BC service or a pre-error proxy response) so
// the caller can still see what went wrong.
//
// Envelope shape:
//
//	{
//	  "code":    "BadRequestError",
//	  "message": "wrong input values, please check your request",
//	  "details": {
//	    "errorSubCode": "DATA_MODEL_NOT_IDENTITY",
//	    "field":        "additional context"
//	  }
//	}
func formatHTTPError(status int, body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("HTTP %d", status)
	}
	var env struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Code == "" {
		// Body isn't a JSON envelope -- emit the raw response so curl-style
		// debugging still works.
		return fmt.Errorf("HTTP %d: %s", status, string(body))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d %s", status, env.Code)
	if env.Message != "" {
		fmt.Fprintf(&b, ": %s", env.Message)
	}
	if subCode, _ := env.Details["errorSubCode"].(string); subCode != "" {
		fmt.Fprintf(&b, " [errorSubCode=%s]", subCode)
	}
	// Surface remaining detail fields (excluding errorSubCode which we
	// already promoted) so per-field validation errors don't get dropped.
	if len(env.Details) > 0 {
		extras := make([]string, 0, len(env.Details))
		for k, v := range env.Details {
			if k == "errorSubCode" {
				continue
			}
			extras = append(extras, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(extras)
		if len(extras) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(extras, ", "))
		}
	}
	return fmt.Errorf("%s", b.String())
}

// Client handles authenticated HTTP requests to the AltScore API.
type Client struct {
	Profile          *config.Profile
	Config           *config.Config
	ProfileName      string
	HTTPClient       *http.Client
	Verbose          bool
	BaseURLOverrides map[string]string // module -> URL overrides (e.g. from --base-url)
}

// New creates a Client from a resolved profile.
func New(cfg *config.Config, profileName string, profile *config.Profile, verbose bool) *Client {
	return &Client{
		Profile:     profile,
		Config:      cfg,
		ProfileName: profileName,
		HTTPClient:  sharedHTTPClient,
		Verbose:     verbose,
	}
}

// Do executes an HTTP request against the given module and path.
// It sets auth and tenant headers, handles JSON encoding, and auto-refreshes
// the token on 401.
func (c *Client) Do(method, module, path string, body any) (json.RawMessage, int, error) {
	return c.DoWithHeaders(method, module, path, body, nil)
}

// DoWithHeaders is like Do but also sets additional HTTP headers on the request.
func (c *Client) DoWithHeaders(method, module, path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	raw, status, err := c.doOnce(method, module, path, body, headers)
	if err != nil {
		return nil, status, err
	}

	// Auto-refresh on 401
	if status == http.StatusUnauthorized {
		if c.Verbose {
			fmt.Fprintln(os.Stderr, "Token expired, refreshing...")
		}
		if err := c.refreshToken(); err != nil {
			return nil, status, fmt.Errorf("token refresh failed: %w", err)
		}
		return c.doOnce(method, module, path, body, headers)
	}

	return raw, status, nil
}

// moduleURL returns the base URL for a module, checking overrides first.
func (c *Client) moduleURL(module string) (string, error) {
	if u, ok := c.BaseURLOverrides[module]; ok {
		return u, nil
	}
	return ModuleURL(c.Profile.Environment, module)
}

// ModuleBaseURL returns the base URL requests for module will hit, honoring
// --base-url overrides. Callers use it as a cache key that tells one backend
// from another (production vs staging vs a local server), which the profile's
// environment alone cannot once an override is in play.
func (c *Client) ModuleBaseURL(module string) (string, error) {
	return c.moduleURL(module)
}

// doOnce performs one request and applies the CLI's default status policy: a
// 401 comes back as (nil, 401, nil) for the caller's refresh, any other >=400
// is folded into err (data nil), an empty 2xx body is nil.
func (c *Client) doOnce(method, module, path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	respBody, status, err := c.doRequest(method, module, path, body, headers)
	if err != nil {
		return nil, status, err
	}

	if status == http.StatusUnauthorized {
		return nil, status, nil
	}

	if status >= 400 {
		return nil, status, formatHTTPError(status, respBody)
	}

	// Some endpoints return no body (204, etc.)
	if len(respBody) == 0 {
		return nil, status, nil
	}

	return json.RawMessage(respBody), status, nil
}

// DoKeepBody is Do for callers that read a STRUCTURED error body: the response
// body comes back for every status and a >=400 status is not folded into err,
// which is non-nil only for transport failures. The 401 auto-refresh still
// applies. `workflows-v2 apply` uses it to render the server's per-node
// findings from a 422 instead of a flattened one-line error.
func (c *Client) DoKeepBody(method, module, path string, body any) (json.RawMessage, int, error) {
	respBody, status, err := c.doRequest(method, module, path, body, nil)
	if err != nil {
		return nil, status, err
	}
	if status == http.StatusUnauthorized {
		if c.Verbose {
			fmt.Fprintln(os.Stderr, "Token expired, refreshing...")
		}
		if err := c.refreshToken(); err != nil {
			return nil, status, fmt.Errorf("token refresh failed: %w", err)
		}
		respBody, status, err = c.doRequest(method, module, path, body, nil)
		if err != nil {
			return nil, status, err
		}
	}
	return json.RawMessage(respBody), status, nil
}

// doRequest builds the request and reads the response; send owns the exchange
// and the connect-phase retry. It returns the body for every status and leaves
// the status policy to its callers. The body is re-derived from the any
// parameter on every attempt, so a replay sends the same bytes.
func (c *Client) doRequest(method, module, path string, body any, headers map[string]string) ([]byte, int, error) {
	baseURL, err := c.moduleURL(module)
	if err != nil {
		return nil, 0, err
	}

	url := baseURL + path

	var data []byte
	if body != nil {
		switch v := body.(type) {
		case json.RawMessage:
			data = v
		case []byte:
			data = v
		default:
			data, err = json.Marshal(body)
			if err != nil {
				return nil, 0, fmt.Errorf("cannot encode request body: %w", err)
			}
		}
	}

	newRequest := func() (*http.Request, error) {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(data)
		}

		req, err := http.NewRequest(method, url, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("cannot create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+c.Profile.AccessToken)
		if c.Profile.TenantID != "" {
			req.Header.Set("X-Tenant-ID", c.Profile.TenantID)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		return req, nil
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "%s %s\n", method, url)
	}

	resp, err := send(c.HTTPClient, newRequest)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cannot read response: %w", err)
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "HTTP %d (%d bytes)\n", resp.StatusCode, len(respBody))
	}

	return respBody, resp.StatusCode, nil
}

func (c *Client) refreshToken() error {
	authURL, err := c.moduleURL("auth")
	if err != nil {
		return err
	}

	token, err := Authenticate(authURL, c.Profile.ClientID, c.Profile.ClientSecret)
	if err != nil {
		return err
	}

	c.Profile.AccessToken = token

	// Persist the new token to config
	if p, ok := c.Config.Profiles[c.ProfileName]; ok {
		p.AccessToken = token
		c.Config.Profiles[c.ProfileName] = p
		if err := config.Save(c.Config); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save refreshed token: %v\n", err)
		}
	}

	return nil
}

// DoRaw executes an HTTP request and returns the raw response body without
// checking content type or parsing JSON. Used for file uploads and other
// non-JSON endpoints.
//
// newBody is a factory, not a reader, because DoRaw replays the request: once
// on a 401 token refresh, and up to retryAttempts times on a connect-phase
// failure inside send. A single io.Reader cannot serve two attempts. The
// transport consumes and closes it on the first, so a replay either fails on
// the closed reader (io.Pipe, as the multipart upload uses) or sends an empty
// body (anything that reports EOF instead), and the upload never happens.
// Buffering the body instead is not an option: the upload is unbounded in size.
// newBody may be nil for a request without a body.
func (c *Client) DoRaw(method, module, path string, newBody func() (io.Reader, error), contentType string) ([]byte, int, error) {
	respBody, status, err := c.doRawOnce(method, module, path, newBody, contentType)
	if err != nil {
		return nil, status, err
	}

	if status == http.StatusUnauthorized {
		if c.Verbose {
			fmt.Fprintln(os.Stderr, "Token expired, refreshing...")
		}
		if err := c.refreshToken(); err != nil {
			return nil, status, fmt.Errorf("token refresh failed: %w", err)
		}
		return c.doRawOnce(method, module, path, newBody, contentType)
	}

	return respBody, status, nil
}

func (c *Client) doRawOnce(method, module, path string, newBody func() (io.Reader, error), contentType string) ([]byte, int, error) {
	baseURL, err := c.moduleURL(module)
	if err != nil {
		return nil, 0, err
	}

	url := baseURL + path

	newRequest := func() (*http.Request, error) {
		var bodyReader io.Reader
		if newBody != nil {
			r, err := newBody()
			if err != nil {
				return nil, err
			}
			bodyReader = r
		}

		req, err := http.NewRequest(method, url, bodyReader)
		if err != nil {
			// The body may be a pipe with a writer goroutine behind it.
			if rc, ok := bodyReader.(io.Closer); ok {
				_ = rc.Close()
			}
			return nil, fmt.Errorf("cannot create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+c.Profile.AccessToken)
		if c.Profile.TenantID != "" {
			req.Header.Set("X-Tenant-ID", c.Profile.TenantID)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		return req, nil
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "%s %s\n", method, url)
	}

	resp, err := send(c.HTTPClient, newRequest)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cannot read response: %w", err)
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "HTTP %d (%d bytes)\n", resp.StatusCode, len(respBody))
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, resp.StatusCode, nil
	}

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, formatHTTPError(resp.StatusCode, respBody)
	}

	return respBody, resp.StatusCode, nil
}
