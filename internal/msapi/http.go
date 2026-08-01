package msapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx response from a Microsoft API. Both the Management
// Activity API and Microsoft Graph return the same `{"error": {...}}`
// envelope, so one type covers both.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	// RetryAfter carries the server's requested backoff, when it sent one.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%s): %s", http.StatusText(e.StatusCode), e.Code, e.Message)
	}
	return fmt.Sprintf("status %d: %s", e.StatusCode, e.Message)
}

// IsForbidden reports whether err is an authorization failure, which almost
// always means a missing or unconsented application permission rather than
// something a retry would fix.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized)
}

// Client performs authenticated, retrying requests against one Microsoft API.
type Client struct {
	tokens     *TokenSource
	http       *http.Client
	log        *slog.Logger
	maxRetries int
}

// NewClient wraps a token source with retry handling.
func NewClient(tokens *TokenSource, hc *http.Client, log *slog.Logger, maxRetries int) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	if log == nil {
		log = slog.Default()
	}
	if maxRetries <= 0 {
		maxRetries = 4
	}
	return &Client{tokens: tokens, http: hc, log: log, maxRetries: maxRetries}
}

// Do performs an authenticated request, retrying throttled (429) and
// transient (5xx) responses with backoff. The caller must close the body of a
// successful response.
func (c *Client) Do(ctx context.Context, method, rawURL string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := Backoff(attempt, lastErr)
			c.log.Debug("retrying request", "attempt", attempt, "delay", delay, "url", rawURL, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		token, err := c.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr := readAPIError(resp)
		resp.Body.Close()
		if !retryable(resp.StatusCode) {
			return nil, apiErr
		}
		lastErr = apiErr
	}
	return nil, fmt.Errorf("giving up after %d retries: %w", c.maxRetries, lastErr)
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

// readAPIError decodes Microsoft's error envelope, falling back to the raw
// body when the response is not the documented shape.
func readAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	out := &APIError{
		StatusCode: resp.StatusCode,
		Message:    Summarise(body),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		out.Code = envelope.Error.Code
		out.Message = envelope.Error.Message
	}
	return out
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Backoff honours a server-supplied Retry-After, otherwise doubles from one
// second up to a 60s ceiling.
func Backoff(attempt int, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, 5*time.Minute)
	}
	d := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
	return min(d, 60*time.Second)
}
