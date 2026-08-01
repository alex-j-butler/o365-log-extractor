package audit

import (
	"encoding/json"
	"testing"
	"time"
)

func normalizeIntuneJSON(t *testing.T, doc string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	n := NewNormalizer()
	n.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	out, err := n.NormalizeIntune(raw)
	if err != nil {
		t.Fatalf("NormalizeIntune: %v", err)
	}
	return out
}

// A representative auditEvent, shaped as Microsoft Graph returns it.
const intuneEvent = `{
	"@odata.type": "#microsoft.graph.auditEvent",
	"id": "b2c3d4e5-0000-4a1b-9c2d-3e4f5a6b7c8d",
	"displayName": "Update DeviceConfiguration",
	"componentName": "DeviceConfiguration",
	"activity": "Patch DeviceConfiguration",
	"activityDateTime": "2026-07-30T09:14:03.1234567Z",
	"activityType": "Patch",
	"activityOperationType": "Patch",
	"activityResult": "Success",
	"correlationId": "9f8e7d6c-0000-4b2a-8c1d-2e3f4a5b6c7d",
	"category": "DeviceConfiguration",
	"actor": {
		"@odata.type": "microsoft.graph.auditActor",
		"auditActorType": "ItPro",
		"userPermissions": ["", "Microsoft.Intune_Organization_Read"],
		"applicationId": "00001111-aaaa-2222-bbbb-3333cccc4444",
		"applicationDisplayName": "Microsoft Intune portal extension",
		"userPrincipalName": "admin@example.com",
		"servicePrincipalName": "",
		"ipAddress": "203.0.113.44",
		"userId": "7a6b5c4d-0000-4e3f-a2b1-c0d9e8f7a6b5"
	},
	"resources": [
		{
			"@odata.type": "microsoft.graph.auditResource",
			"displayName": "Windows 10 Baseline",
			"type": "Windows10GeneralConfiguration",
			"resourceId": "1a2b3c4d-0000-4f5e-8a9b-0c1d2e3f4a5b",
			"modifiedProperties": [
				{
					"@odata.type": "microsoft.graph.auditProperty",
					"displayName": "PasswordMinimumLength",
					"oldValue": "4",
					"newValue": "8"
				},
				{
					"displayName": "PasswordRequired",
					"oldValue": "false",
					"newValue": "true"
				}
			]
		}
	]
}`

func TestNormalizeIntuneCommonCore(t *testing.T) {
	out := normalizeIntuneJSON(t, intuneEvent)

	// The synthesised common core is what makes Intune and Office 365
	// records queryable with one set of fields.
	checks := map[string]any{
		"_time":          "2026-07-30T09:14:03.1234567Z",
		"Workload":       "MicrosoftIntune",
		"RecordTypeName": "IntuneDeviceConfiguration",
		"Operation":      "Patch DeviceConfiguration",
		"UserId":         "admin@example.com",
		"ResultStatus":   "Success",
		"ClientIP":       "203.0.113.44",
	}
	for field, want := range checks {
		if got := out[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}

	want := "Patch DeviceConfiguration user=admin@example.com workload=MicrosoftIntune result=Success component=DeviceConfiguration ip=203.0.113.44"
	if got := out["_msg"]; got != want {
		t.Errorf("_msg = %v, want %v", got, want)
	}
}

func TestNormalizeIntunePreservesNativeFields(t *testing.T) {
	out := normalizeIntuneJSON(t, intuneEvent)

	checks := map[string]any{
		"id":                           "b2c3d4e5-0000-4a1b-9c2d-3e4f5a6b7c8d",
		"activity":                     "Patch DeviceConfiguration",
		"activityResult":               "Success",
		"category":                     "DeviceConfiguration",
		"componentName":                "DeviceConfiguration",
		"actor.userPrincipalName":      "admin@example.com",
		"actor.auditActorType":         "ItPro",
		"actor.ipAddress":              "203.0.113.44",
		"actor.applicationDisplayName": "Microsoft Intune portal extension",
	}
	for field, want := range checks {
		if got := out[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}

	// Scalar arrays stay greppable rather than becoming opaque JSON.
	if got, want := out["actor.userPermissions"], ",Microsoft.Intune_Organization_Read"; got != want {
		t.Errorf("actor.userPermissions = %v, want %v", got, want)
	}
}

func TestNormalizeIntuneExpandsResources(t *testing.T) {
	out := normalizeIntuneJSON(t, intuneEvent)

	checks := map[string]any{
		"resources.0.displayName": "Windows 10 Baseline",
		"resources.0.type":        "Windows10GeneralConfiguration",
		"resources.0.resourceId":  "1a2b3c4d-0000-4f5e-8a9b-0c1d2e3f4a5b",
		// The whole point: changed settings become queryable fields.
		"resources.0.modifiedProperties.PasswordMinimumLength":     "8",
		"resources.0.modifiedProperties.PasswordMinimumLength.old": "4",
		"resources.0.modifiedProperties.PasswordRequired":          "true",
		"resources.0.modifiedProperties.PasswordRequired.old":      "false",
	}
	for field, want := range checks {
		if got := out[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}

	if _, ok := out["resources"]; ok {
		t.Error("raw resources array should be expanded, not stored verbatim")
	}
}

func TestNormalizeIntuneDropsODataAnnotations(t *testing.T) {
	out := normalizeIntuneJSON(t, intuneEvent)
	for field := range out {
		if len(field) >= 7 && field[:7] == "@odata." {
			t.Errorf("unexpected OData annotation field %q", field)
		}
		if field == "actor.@odata.type" || field == "resources.0.@odata.type" {
			t.Errorf("unexpected nested OData annotation %q", field)
		}
	}
}

func TestNormalizeIntuneFallsBackForActorAndCategory(t *testing.T) {
	out := normalizeIntuneJSON(t, `{
		"id": "no-upn",
		"activityDateTime": "2026-07-30T09:14:03Z",
		"displayName": "Enroll device",
		"activityResult": "Fail",
		"actor": {"servicePrincipalName": "sync-service"}
	}`)

	// No category -> a stable stream label rather than an empty one.
	if got, want := out["RecordTypeName"], "IntuneAuditEvent"; got != want {
		t.Errorf("RecordTypeName = %v, want %v", got, want)
	}
	// No activity -> falls back to displayName.
	if got, want := out["Operation"], "Enroll device"; got != want {
		t.Errorf("Operation = %v, want %v", got, want)
	}
	// No userPrincipalName -> falls back to the service principal.
	if got, want := out["UserId"], "sync-service"; got != want {
		t.Errorf("UserId = %v, want %v", got, want)
	}
	if _, ok := out["ClientIP"]; ok {
		t.Error("ClientIP should be absent when the actor has no IP")
	}
}

func TestNormalizeIntuneInfersMissingTime(t *testing.T) {
	out := normalizeIntuneJSON(t, `{"id": "x", "activity": "Delete"}`)

	if out["_time_inferred"] != true {
		t.Error("expected _time_inferred to be set")
	}
	if got, want := out["_time"], "1970-01-01T00:00:00Z"; got != want {
		t.Errorf("_time = %v, want %v", got, want)
	}
}

func TestIntuneRecordTypeNameSanitisesCategory(t *testing.T) {
	// Categories with spaces must not produce a field value that needs
	// escaping in LogsQL.
	if got, want := intuneRecordTypeName("Remote Actions"), "IntuneRemote_Actions"; got != want {
		t.Errorf("intuneRecordTypeName = %v, want %v", got, want)
	}
	if got, want := intuneRecordTypeName("  "), "IntuneAuditEvent"; got != want {
		t.Errorf("intuneRecordTypeName = %v, want %v", got, want)
	}
}
