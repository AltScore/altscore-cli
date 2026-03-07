package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/AltScore/altscore-cli/internal/config"
)

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
		HTTPClient:  &http.Client{},
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

func (c *Client) doOnce(method, module, path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	baseURL, err := c.moduleURL(module)
	if err != nil {
		return nil, 0, err
	}

	url := baseURL + path

	var bodyReader io.Reader
	if body != nil {
		switch v := body.(type) {
		case json.RawMessage:
			bodyReader = bytes.NewReader(v)
		case []byte:
			bodyReader = bytes.NewReader(v)
		default:
			data, err := json.Marshal(body)
			if err != nil {
				return nil, 0, fmt.Errorf("cannot encode request body: %w", err)
			}
			bodyReader = bytes.NewReader(data)
		}
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create request: %w", err)
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

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "%s %s\n", method, url)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
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
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Some endpoints return no body (204, etc.)
	if len(respBody) == 0 {
		return nil, resp.StatusCode, nil
	}

	return json.RawMessage(respBody), resp.StatusCode, nil
}

func (c *Client) refreshToken() error {
	authURL, err := ModuleURL(c.Profile.Environment, "auth")
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
func (c *Client) DoRaw(method, module, path string, bodyReader io.Reader, contentType string) ([]byte, int, error) {
	respBody, status, err := c.doRawOnce(method, module, path, bodyReader, contentType)
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
		return c.doRawOnce(method, module, path, bodyReader, contentType)
	}

	return respBody, status, nil
}

func (c *Client) doRawOnce(method, module, path string, bodyReader io.Reader, contentType string) ([]byte, int, error) {
	baseURL, err := c.moduleURL(module)
	if err != nil {
		return nil, 0, err
	}

	url := baseURL + path

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.Profile.AccessToken)
	if c.Profile.TenantID != "" {
		req.Header.Set("X-Tenant-ID", c.Profile.TenantID)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "%s %s\n", method, url)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
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
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, resp.StatusCode, nil
}
