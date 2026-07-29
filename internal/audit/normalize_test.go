package audit

import (
	"encoding/json"
	"testing"
	"time"
)

func normalizeJSON(t *testing.T, doc string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	n := NewNormalizer()
	n.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	out, err := n.Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return out
}

func TestNormalizeSetsSpecialFields(t *testing.T) {
	out := normalizeJSON(t, `{
		"Id": "8a1b0f7e-0000-4d0f-bb1a-1f5f7f6b1a2c",
		"RecordType": 15,
		"CreationTime": "2026-07-28T04:15:22",
		"Operation": "UserLoggedIn",
		"UserType": 0,
		"UserId": "alex@example.com",
		"Workload": "AzureActiveDirectory",
		"ResultStatus": "Success",
		"ClientIP": "203.0.113.10"
	}`)

	if got, want := out["_time"], "2026-07-28T04:15:22Z"; got != want {
		t.Errorf("_time = %v, want %v", got, want)
	}
	if _, inferred := out["_time_inferred"]; inferred {
		t.Error("_time_inferred set for a parseable CreationTime")
	}
	if got, want := out["RecordTypeName"], "AzureActiveDirectoryStsLogon"; got != want {
		t.Errorf("RecordTypeName = %v, want %v", got, want)
	}
	if got, want := out["UserTypeName"], "Regular"; got != want {
		t.Errorf("UserTypeName = %v, want %v", got, want)
	}
	want := "UserLoggedIn user=alex@example.com workload=AzureActiveDirectory result=Success ip=203.0.113.10"
	if got := out["_msg"]; got != want {
		t.Errorf("_msg = %v, want %v", got, want)
	}
}

func TestNormalizeFallsBackWhenTimeMissing(t *testing.T) {
	out := normalizeJSON(t, `{"Operation": "FileAccessed", "CreationTime": "not-a-date"}`)

	if out["_time_inferred"] != true {
		t.Error("expected _time_inferred to be set")
	}
	if got, want := out["_time"], "1970-01-01T00:00:00Z"; got != want {
		t.Errorf("_time = %v, want %v", got, want)
	}
}

func TestNormalizeFlattensNestedObjects(t *testing.T) {
	out := normalizeJSON(t, `{
		"CreationTime": "2026-07-28T04:15:22",
		"Operation": "FileAccessed",
		"SharePointMetaData": {"Site": {"Url": "https://example.sharepoint.com"}},
		"AffectedItems": ["a.docx", "b.docx"],
		"Nulled": null
	}`)

	if got, want := out["SharePointMetaData.Site.Url"], "https://example.sharepoint.com"; got != want {
		t.Errorf("nested field = %v, want %v", got, want)
	}
	if got, want := out["AffectedItems"], "a.docx,b.docx"; got != want {
		t.Errorf("scalar array = %v, want %v", got, want)
	}
	if _, ok := out["Nulled"]; ok {
		t.Error("null field should be dropped")
	}
}

func TestNormalizeExpandsNameValuePairs(t *testing.T) {
	out := normalizeJSON(t, `{
		"CreationTime": "2026-07-28T04:15:22",
		"Operation": "Set-Mailbox",
		"Parameters": [
			{"Name": "Identity", "Value": "alex@example.com"},
			{"Name": "Forwarding SMTP Address", "Value": "attacker@evil.example"}
		],
		"ModifiedProperties": [
			{"Name": "Role.DisplayName", "NewValue": "Global Administrator", "OldValue": "User"}
		]
	}`)

	if got, want := out["Parameters.Identity"], "alex@example.com"; got != want {
		t.Errorf("Parameters.Identity = %v, want %v", got, want)
	}
	if got, want := out["Parameters.Forwarding_SMTP_Address"], "attacker@evil.example"; got != want {
		t.Errorf("sanitised key = %v, want %v", got, want)
	}
	if got, want := out["ModifiedProperties.Role.DisplayName"], "Global Administrator"; got != want {
		t.Errorf("ModifiedProperties new value = %v, want %v", got, want)
	}
	if got, want := out["ModifiedProperties.Role.DisplayName.old"], "User"; got != want {
		t.Errorf("ModifiedProperties old value = %v, want %v", got, want)
	}
}

func TestNormalizeUnwrapsAuditDataString(t *testing.T) {
	out := normalizeJSON(t, `{
		"CreationDate": "2026-07-28T04:15:22",
		"RecordType": "AzureActiveDirectoryStsLogon",
		"AuditData": "{\"CreationTime\":\"2026-07-28T04:15:22\",\"Operation\":\"UserLoggedIn\",\"Workload\":\"AzureActiveDirectory\",\"UserId\":\"alex@example.com\"}"
	}`)

	if got, want := out["Operation"], "UserLoggedIn"; got != want {
		t.Errorf("Operation = %v, want %v", got, want)
	}
	if got, want := out["_time"], "2026-07-28T04:15:22Z"; got != want {
		t.Errorf("_time = %v, want %v", got, want)
	}
	// The outer column survives, and a symbolic RecordType passes through.
	if got, want := out["CreationDate"], "2026-07-28T04:15:22"; got != want {
		t.Errorf("CreationDate = %v, want %v", got, want)
	}
	if got, want := out["RecordTypeName"], "AzureActiveDirectoryStsLogon"; got != want {
		t.Errorf("RecordTypeName = %v, want %v", got, want)
	}
}

func TestRecordTypeNameUnknown(t *testing.T) {
	if got, want := RecordTypeName(99999), "RecordType_99999"; got != want {
		t.Errorf("RecordTypeName = %v, want %v", got, want)
	}
}
