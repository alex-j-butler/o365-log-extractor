// Package intune is a minimal Microsoft Graph client for Intune audit
// events (GET /deviceManagement/auditEvents).
package intune

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alex-j-butler/o365-log-extractor/internal/msapi"
)

const (
	// DefaultAPIVersion is the stable Graph version. auditEvents is
	// available on v1.0; "beta" adds a few actor and resource properties.
	DefaultAPIVersion = "v1.0"
	// MaxRetention is roughly how far back Graph exposes Intune audit
	// events. Microsoft documents "two years of audit events".
	MaxRetention = 2 * 365 * 24 * time.Hour
	// defaultPageSize is the OData $top value; Graph caps page sizes well
	// below this for some resources and will silently return fewer.
	defaultPageSize = 1000
	// maxPages bounds @odata.nextLink following so a misbehaving server
	// cannot spin forever.
	maxPages = 10000
	// graphTimeLayout is the ISO 8601 form accepted in $filter comparisons
	// against Edm.DateTimeOffset.
	graphTimeLayout = "2006-01-02T15:04:05Z"
)

// Client reads Intune audit events from Microsoft Graph.
type Client struct {
	api        *msapi.Client
	baseURL    string
	apiVersion string
	pageSize   int
	log        *slog.Logger
}

// Options configures a Client.
type Options struct {
	Cloud        msapi.Cloud
	TenantID     string
	ClientID     string
	ClientSecret string
	// APIVersion selects the Graph version, "v1.0" or "beta".
	APIVersion string
	PageSize   int
	HTTPClient *http.Client
	Logger     *slog.Logger
	MaxRetries int
}

// New builds a Client from Options.
func New(opts Options) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	apiVersion := strings.Trim(strings.TrimSpace(opts.APIVersion), "/")
	if apiVersion == "" {
		apiVersion = DefaultAPIVersion
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}

	tokens := msapi.NewTokenSource(
		opts.Cloud.LoginURL, opts.TenantID, opts.ClientID, opts.ClientSecret,
		opts.Cloud.GraphURL, opts.HTTPClient,
	)
	return &Client{
		api:        msapi.NewClient(tokens, opts.HTTPClient, logger, opts.MaxRetries),
		baseURL:    strings.TrimSuffix(opts.Cloud.GraphURL, "/"),
		apiVersion: apiVersion,
		pageSize:   pageSize,
		log:        logger,
	}
}

// ListAuditEvents streams every Intune audit event with an activityDateTime
// in (start, end], following @odata.nextLink paging. fn is called once per
// event; returning an error from it aborts the walk.
func (c *Client) ListAuditEvents(ctx context.Context, start, end time.Time, fn func(map[string]any) error) error {
	start, end = start.UTC(), end.UTC()
	if earliest := time.Now().UTC().Add(-MaxRetention); start.Before(earliest) {
		c.log.Warn("start time predates the Intune audit retention window; reading from the earliest available event",
			"requested", start, "clamped", earliest)
		start = earliest
	}
	if !start.Before(end) {
		return nil
	}

	next := c.auditEventsURL(start, end)
	for page := 0; next != ""; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if page >= maxPages {
			return fmt.Errorf("aborting after %d pages: the server keeps returning @odata.nextLink", maxPages)
		}

		resp, err := c.api.Do(ctx, http.MethodGet, next)
		if err != nil {
			return err
		}

		// Pages are bounded by $top, so decoding one at a time is fine and
		// keeps the nextLink handling simple.
		var body struct {
			Value    []map[string]any `json:"value"`
			NextLink string           `json:"@odata.nextLink"`
		}
		dec := json.NewDecoder(resp.Body)
		dec.UseNumber()
		err = dec.Decode(&body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode audit events: %w", err)
		}

		for _, event := range body.Value {
			if err := fn(event); err != nil {
				return err
			}
		}
		c.log.Debug("read audit event page", "page", page, "events", len(body.Value), "more", body.NextLink != "")

		// nextLink already carries the filter and skip token, so it is
		// followed verbatim.
		next = body.NextLink
	}
	return nil
}

// auditEventsURL builds the first page's URL. The window is half-open so
// that a cursor can be advanced to `end` without re-reading its boundary
// event on the next poll.
func (c *Client) auditEventsURL(start, end time.Time) string {
	filter := fmt.Sprintf("activityDateTime gt %s and activityDateTime le %s",
		start.Format(graphTimeLayout), end.Format(graphTimeLayout))

	query := url.Values{
		"$filter": {filter},
		"$top":    {strconv.Itoa(c.pageSize)},
	}
	return fmt.Sprintf("%s/%s/deviceManagement/auditEvents?%s", c.baseURL, c.apiVersion, encodeQuery(query))
}

// encodeQuery percent-encodes spaces rather than using '+'. OData $filter
// expressions are full of spaces and Graph is happier with %20.
func encodeQuery(v url.Values) string {
	return strings.ReplaceAll(v.Encode(), "+", "%20")
}
