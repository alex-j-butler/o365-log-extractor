package audit

import (
	"strconv"
	"strings"
	"time"
)

// IntuneWorkload is the synthetic Workload value given to every Intune audit
// event, so that Intune and Office 365 records share one stream layout.
const IntuneWorkload = "MicrosoftIntune"

// NormalizeIntune flattens a Microsoft Graph auditEvent
// (GET /deviceManagement/auditEvents) into a VictoriaLogs document.
//
// Graph names its fields quite differently from the Office 365 unified audit
// schema. Every native Graph field is preserved as-is, and a small common
// core (Workload, RecordTypeName, Operation, UserId, ResultStatus, ClientIP)
// is synthesised alongside so that a single query, and a single set of
// -vl-stream-fields, works across both feeds.
func (n *Normalizer) NormalizeIntune(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return nil, errNilRecord
	}

	// resources is an array of objects with a nested {displayName, oldValue,
	// newValue} list; it is expanded separately so the changed properties
	// become real fields instead of an opaque JSON string.
	rest := make(map[string]any, len(raw))
	var resources []any
	for k, v := range raw {
		if k == "resources" {
			if items, ok := v.([]any); ok {
				resources = items
				continue
			}
		}
		rest[k] = v
	}

	out := make(map[string]any, len(raw)+12)
	n.flatten("", rest, out, 0)
	n.expandResources(resources, out)

	if ts, ok := parseTime(raw["activityDateTime"]); ok {
		out["_time"] = ts.UTC().Format(time.RFC3339Nano)
	} else {
		out["_time"] = n.now().UTC().Format(time.RFC3339Nano)
		out["_time_inferred"] = true
	}

	actor, _ := raw["actor"].(map[string]any)
	operation := firstNonEmpty(
		str(raw["activity"]),
		str(raw["displayName"]),
		str(raw["activityType"]),
		"IntuneAuditEvent",
	)
	user := firstNonEmpty(
		str(actor["userPrincipalName"]),
		str(actor["servicePrincipalName"]),
		str(actor["applicationDisplayName"]),
		str(actor["userId"]),
	)

	// The common core. Graph has no fields by these names, so nothing native
	// is overwritten.
	out["Workload"] = IntuneWorkload
	out["RecordTypeName"] = intuneRecordTypeName(str(raw["category"]))
	out["Operation"] = operation
	if user != "" {
		out["UserId"] = user
	}
	if result := str(raw["activityResult"]); result != "" {
		out["ResultStatus"] = result
	}
	if ip := str(actor["ipAddress"]); ip != "" {
		out["ClientIP"] = ip
	}

	msg := newMessage(operation)
	msg.add("user", user)
	msg.add("workload", IntuneWorkload)
	msg.add("result", str(raw["activityResult"]))
	msg.add("component", str(raw["componentName"]))
	msg.add("ip", str(actor["ipAddress"]))
	out["_msg"] = msg.String()

	for k, v := range n.Extra {
		out[k] = v
	}
	return out, nil
}

// intuneRecordTypeName derives a low-cardinality stream label from the audit
// category, mirroring how RecordTypeName labels Office 365 records. The
// "Intune" prefix keeps the value from colliding with an O365 record type.
func intuneRecordTypeName(category string) string {
	category = sanitizeKey(strings.TrimSpace(category))
	if category == "" || category == "_" {
		return "IntuneAuditEvent"
	}
	return "Intune" + category
}

// expandResources turns the auditResource collection into indexed fields:
// resources.0.displayName, resources.0.modifiedProperties.<name> and
// resources.0.modifiedProperties.<name>.old.
func (n *Normalizer) expandResources(items []any, out map[string]any) {
	for i, item := range items {
		prefix := "resources." + strconv.Itoa(i)

		resource, ok := item.(map[string]any)
		if !ok {
			out[prefix] = jsonString(item)
			continue
		}

		rest := make(map[string]any, len(resource))
		for k, v := range resource {
			if k == "modifiedProperties" {
				expandAuditProperties(join(prefix, "modifiedProperties"), v, out)
				continue
			}
			rest[k] = v
		}
		n.flatten(prefix, rest, out, 1)
	}
}

// expandAuditProperties expands a [{displayName, oldValue, newValue}] list.
// It is Graph's equivalent of the Office 365 ModifiedProperties array, but
// with lower-cased key names.
func expandAuditProperties(prefix string, v any, out map[string]any) {
	items, ok := v.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		property, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(str(property["displayName"]))
		if name == "" {
			continue
		}
		key := join(prefix, sanitizeKey(name))
		if newValue := str(property["newValue"]); newValue != "" {
			out[key] = newValue
		}
		if oldValue := str(property["oldValue"]); oldValue != "" {
			out[key+".old"] = oldValue
		}
	}
}
