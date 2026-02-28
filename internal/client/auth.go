package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
}

// Authenticate exchanges client credentials for an access token.
func Authenticate(authURL, clientID, clientSecret string) (string, error) {
	body := tokenRequest{
		GrantType:    "client_credentials",
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("cannot encode auth request: %w", err)
	}

	resp, err := http.Post(authURL+"/oauth/token", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("auth request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("cannot read auth response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var tok tokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return "", fmt.Errorf("cannot parse auth response: %w", err)
	}

	if tok.AccessToken == "" {
		return "", fmt.Errorf("auth response did not contain access_token")
	}

	return tok.AccessToken, nil
}
