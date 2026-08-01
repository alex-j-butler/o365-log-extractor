package msapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource issues and caches app-only access tokens using the OAuth2
// client-credentials grant. It is safe for concurrent use.
type TokenSource struct {
	tenantID     string
	clientID     string
	clientSecret string
	loginURL     string
	scope        string
	httpClient   *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewTokenSource builds a token source for one tenant and one API. The
// resource is the API's base URL (e.g. https://graph.microsoft.com); the
// v2.0 `.default` scope is derived from it, so each API gets its own
// independently cached token.
func NewTokenSource(loginURL, tenantID, clientID, clientSecret, resource string, hc *http.Client) *TokenSource {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &TokenSource{
		tenantID:     tenantID,
		clientID:     clientID,
		clientSecret: clientSecret,
		loginURL:     strings.TrimSuffix(loginURL, "/"),
		scope:        strings.TrimSuffix(resource, "/") + "/.default",
		httpClient:   hc,
	}
}

// Token returns a valid access token, refreshing it when it is within a
// minute of expiry.
func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.token != "" && time.Now().Before(ts.expiry.Add(-time.Minute)) {
		return ts.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {ts.clientID},
		"client_secret": {ts.clientSecret},
		"scope":         {ts.scope},
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", ts.loginURL, url.PathEscape(ts.tenantID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ts.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request for %s failed: %s: %s", ts.scope, resp.Status, Summarise(body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token response for %s contained no access_token", ts.scope)
	}

	ts.token = out.AccessToken
	ts.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return ts.token, nil
}

// Summarise trims a response body so credentials-adjacent responses do not
// flood the logs.
func Summarise(body []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(body))
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
