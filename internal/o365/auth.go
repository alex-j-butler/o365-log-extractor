// Package o365 is a minimal client for the Office 365 Management Activity
// API: OAuth2 client-credentials auth, subscription management, content
// listing and blob retrieval.
package o365

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

// Cloud describes the Microsoft cloud instance to talk to. The Management
// Activity API is hosted on different endpoints per sovereign cloud.
type Cloud struct {
	Name     string
	LoginURL string
	APIURL   string
}

var clouds = map[string]Cloud{
	"commercial": {"commercial", "https://login.microsoftonline.com", "https://manage.office.com"},
	"gcc":        {"gcc", "https://login.microsoftonline.com", "https://manage.office.com"},
	"gcchigh":    {"gcchigh", "https://login.microsoftonline.us", "https://manage.office365.us"},
	"dod":        {"dod", "https://login.microsoftonline.us", "https://manage.protection.apps.mil"},
}

// LookupCloud resolves a cloud by name (commercial, gcc, gcchigh, dod).
func LookupCloud(name string) (Cloud, error) {
	c, ok := clouds[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Cloud{}, fmt.Errorf("unknown cloud %q (want one of: commercial, gcc, gcchigh, dod)", name)
	}
	return c, nil
}

// TokenSource issues and caches app-only access tokens using the OAuth2
// client-credentials grant.
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

// NewTokenSource builds a token source for the given tenant and cloud.
func NewTokenSource(cloud Cloud, tenantID, clientID, clientSecret string, hc *http.Client) *TokenSource {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &TokenSource{
		tenantID:     tenantID,
		clientID:     clientID,
		clientSecret: clientSecret,
		loginURL:     strings.TrimSuffix(cloud.LoginURL, "/"),
		scope:        strings.TrimSuffix(cloud.APIURL, "/") + "/.default",
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
		return "", fmt.Errorf("token request failed: %s: %s", resp.Status, summarise(body))
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
		return "", fmt.Errorf("token response contained no access_token")
	}

	ts.token = out.AccessToken
	ts.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return ts.token, nil
}

// summarise trims an error body so credentials-adjacent responses do not
// flood the logs.
func summarise(body []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(body))
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
