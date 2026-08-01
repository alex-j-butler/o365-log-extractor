// Package audit parses Office 365 unified audit records and reshapes them
// into flat documents suitable for ingestion into VictoriaLogs.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxFlattenDepth bounds recursion when flattening nested audit payloads.
const maxFlattenDepth = 12

// errNilRecord is returned for a nil record so callers can count skips
// without string matching.
var errNilRecord = errors.New("nil audit record")

// nameValueFields are audit properties that carry a list of {Name, Value}
// pairs rather than a plain object. They are far more useful when expanded
// into real fields, e.g. Parameters.Identity="alex@example.com".
var nameValueFields = map[string]bool{
	"Parameters":         true,
	"ExtendedProperties": true,
	"ModifiedProperties": true,
}

// Normalizer converts raw audit records into VictoriaLogs documents.
type Normalizer struct {
	// DecodeEnums adds RecordTypeName and UserTypeName derived fields.
	DecodeEnums bool
	// ExpandNameValue turns {Name, Value} arrays into individual fields.
	ExpandNameValue bool
	// Extra fields are added to every record (e.g. source=export.json).
	Extra map[string]string
	// Now is used to timestamp records with an unparseable CreationTime.
	// Defaults to time.Now when nil.
	Now func() time.Time
}

// NewNormalizer returns a Normalizer with the recommended defaults.
func NewNormalizer() *Normalizer {
	return &Normalizer{DecodeEnums: true, ExpandNameValue: true}
}

// Normalize flattens a single audit record and attaches the VictoriaLogs
// special fields `_time` and `_msg`. The returned map is safe to marshal
// directly as one JSON line.
func (n *Normalizer) Normalize(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return nil, errNilRecord
	}
	raw = unwrapAuditData(raw)

	out := make(map[string]any, len(raw)+8)
	n.flatten("", raw, out, 0)

	if ts, ok := parseTime(raw["CreationTime"]); ok {
		out["_time"] = ts.UTC().Format(time.RFC3339Nano)
	} else {
		out["_time"] = n.now().UTC().Format(time.RFC3339Nano)
		out["_time_inferred"] = true
	}

	if n.DecodeEnums {
		if name, ok := enumName(raw["RecordType"], RecordTypeName); ok {
			out["RecordTypeName"] = name
		}
		if name, ok := enumName(raw["UserType"], UserTypeName); ok {
			out["UserTypeName"] = name
		}
	}

	out["_msg"] = buildMessage(raw)
	for k, v := range n.Extra {
		out[k] = v
	}
	return out, nil
}

func (n *Normalizer) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// unwrapAuditData handles exports (notably Purview CSV and some SIEM
// forwarders) that nest the real record inside an AuditData JSON string or
// object. Columns from the outer record are preserved unless the inner
// record already defines them.
func unwrapAuditData(raw map[string]any) map[string]any {
	inner, ok := asObject(raw["AuditData"])
	if !ok {
		return raw
	}
	merged := make(map[string]any, len(inner)+len(raw))
	for k, v := range inner {
		merged[k] = v
	}
	for k, v := range raw {
		if k == "AuditData" {
			continue
		}
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return merged
}

// asObject returns v as a JSON object, decoding it first if it is a string
// holding JSON.
func asObject(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case string:
		s := strings.TrimSpace(t)
		if !strings.HasPrefix(s, "{") {
			return nil, false
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, false
		}
		return m, true
	}
	return nil, false
}

// flatten writes v into out using dotted key paths. Scalars are kept as-is so
// VictoriaLogs can index numbers and booleans natively; arrays that are not
// Name/Value pairs are encoded as a JSON string.
func (n *Normalizer) flatten(prefix string, v any, out map[string]any, depth int) {
	if depth > maxFlattenDepth {
		out[prefix] = jsonString(v)
		return
	}

	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return
		}
		for k, child := range t {
			// Graph echoes its type annotations back on every object;
			// they carry no audit information.
			if strings.HasPrefix(k, "@odata.") {
				continue
			}
			key := join(prefix, sanitizeKey(k))
			if n.ExpandNameValue && nameValueFields[k] {
				if expandNameValue(key, child, out) {
					continue
				}
			}
			n.flatten(key, child, out, depth+1)
		}
	case []any:
		if prefix == "" {
			out["_records"] = jsonString(t)
			return
		}
		if s, ok := scalarSlice(t); ok {
			out[prefix] = strings.Join(s, ",")
			return
		}
		out[prefix] = jsonString(t)
	case nil:
		// Drop nulls: an absent field is cheaper to store and query than an
		// empty one.
	default:
		if prefix == "" {
			out["_msg"] = fmt.Sprint(t)
			return
		}
		out[prefix] = t
	}
}

// expandNameValue turns [{"Name":"x","Value":"y"}] into prefix.x = "y".
// It reports whether the value had the expected shape.
func expandNameValue(prefix string, v any, out map[string]any) bool {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	expanded := make(map[string]any, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return false
		}
		name, ok := obj["Name"].(string)
		if !ok || name == "" {
			return false
		}
		key := join(prefix, sanitizeKey(name))
		switch {
		case obj["Value"] != nil:
			expanded[key] = obj["Value"]
		case obj["NewValue"] != nil || obj["OldValue"] != nil:
			if obj["NewValue"] != nil {
				expanded[key] = obj["NewValue"]
			}
			if obj["OldValue"] != nil {
				expanded[key+".old"] = obj["OldValue"]
			}
		default:
			return false
		}
	}
	for k, val := range expanded {
		if s, ok := val.(string); ok {
			out[k] = s
			continue
		}
		out[k] = jsonString(val)
	}
	return true
}

// scalarSlice renders an array of scalars as strings, so that arrays such as
// AffectedItems stay greppable instead of becoming opaque JSON.
func scalarSlice(items []any) ([]string, bool) {
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch t := item.(type) {
		case string:
			if strings.ContainsAny(t, ",\n") {
				return nil, false
			}
			out = append(out, t)
		case float64, bool, json.Number:
			out = append(out, fmt.Sprint(t))
		default:
			return nil, false
		}
	}
	return out, true
}

// message renders the `_msg` summary line: an operation name followed by
// key=value context. It is what VictoriaLogs free-text search matches
// against, so both feeds build it the same way.
type message struct {
	b strings.Builder
}

func newMessage(operation string) *message {
	m := &message{}
	m.b.WriteString(operation)
	return m
}

// add appends ` key=value`, skipping empty values.
func (m *message) add(key, value string) {
	if value == "" {
		return
	}
	m.b.WriteByte(' ')
	m.b.WriteString(key)
	m.b.WriteByte('=')
	m.b.WriteString(value)
}

func (m *message) String() string { return m.b.String() }

// buildMessage renders the summary for an Office 365 unified audit record.
func buildMessage(raw map[string]any) string {
	op := str(raw["Operation"])
	if op == "" {
		op = "AuditRecord"
	}

	m := newMessage(op)
	m.add("user", firstNonEmpty(str(raw["UserId"]), str(raw["UserKey"])))
	m.add("workload", str(raw["Workload"]))
	m.add("result", str(raw["ResultStatus"]))
	m.add("object", str(raw["ObjectId"]))
	m.add("ip", firstNonEmpty(str(raw["ClientIP"]), str(raw["ClientIPAddress"])))
	return m.String()
}

// parseTime accepts the timestamp formats seen across the Management
// Activity API, Purview exports and hand-edited files.
func parseTime(v any) (time.Time, bool) {
	s := strings.TrimSpace(str(v))
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999", // API blobs: UTC, no zone suffix
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"1/2/2006 3:04:05 PM",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			// Layouts without a zone are documented as UTC; time.Parse
			// already yields UTC for those, so this is a no-op for them.
			return t, true
		}
	}
	return time.Time{}, false
}

// enumName resolves a RecordType/UserType value that may arrive as a number
// or, in CSV exports, as the symbolic name already.
func enumName(v any, lookup func(int) string) (string, bool) {
	switch t := v.(type) {
	case float64:
		return lookup(int(t)), true
	case int:
		return lookup(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return lookup(int(i)), true
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return "", false
		}
		if i, err := strconv.Atoi(s); err == nil {
			return lookup(i), true
		}
		return s, true
	}
	return "", false
}

// sanitizeKey keeps field names usable in LogsQL: no spaces, quotes or
// separators that would need escaping at query time.
func sanitizeKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '"', '\'', '|', ':', '=', '{', '}', '(', ')', '[', ']', ',':
			return '_'
		}
		return r
	}, k)
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
