// Package msapi holds the HTTP plumbing shared by the Microsoft APIs this
// tool talks to: sovereign cloud endpoints, OAuth2 client-credentials tokens,
// and a retrying HTTP client that understands Microsoft's error envelope and
// throttling headers.
package msapi

import (
	"fmt"
	"strings"
)

// Cloud describes one Microsoft cloud instance. Each API is hosted on a
// different host per sovereign cloud, so they are resolved together.
type Cloud struct {
	Name string
	// LoginURL is the Entra ID authority issuing access tokens.
	LoginURL string
	// ManagementURL hosts the Office 365 Management Activity API.
	ManagementURL string
	// GraphURL hosts Microsoft Graph.
	GraphURL string
}

var clouds = map[string]Cloud{
	"commercial": {
		Name:          "commercial",
		LoginURL:      "https://login.microsoftonline.com",
		ManagementURL: "https://manage.office.com",
		GraphURL:      "https://graph.microsoft.com",
	},
	"gcc": {
		Name:          "gcc",
		LoginURL:      "https://login.microsoftonline.com",
		ManagementURL: "https://manage.office.com",
		GraphURL:      "https://graph.microsoft.com",
	},
	"gcchigh": {
		Name:          "gcchigh",
		LoginURL:      "https://login.microsoftonline.us",
		ManagementURL: "https://manage.office365.us",
		GraphURL:      "https://graph.microsoft.us",
	},
	"dod": {
		Name:          "dod",
		LoginURL:      "https://login.microsoftonline.us",
		ManagementURL: "https://manage.protection.apps.mil",
		GraphURL:      "https://dod-graph.microsoft.us",
	},
}

// LookupCloud resolves a cloud by name (commercial, gcc, gcchigh, dod).
func LookupCloud(name string) (Cloud, error) {
	c, ok := clouds[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Cloud{}, fmt.Errorf("unknown cloud %q (want one of: commercial, gcc, gcchigh, dod)", name)
	}
	return c, nil
}
