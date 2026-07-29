package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// RecordFunc is called once per parsed audit record. Returning an error
// aborts parsing.
type RecordFunc func(raw map[string]any) error

// ReadFile parses an audit log export from disk and invokes fn for every
// record. Pass "-" to read stdin. Gzip-compressed input is detected and
// decompressed transparently.
func ReadFile(path string, fn RecordFunc) error {
	if path == "-" {
		return ReadStream(os.Stdin, fn)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := ReadStream(f, fn); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ReadStream parses audit records from r, auto-detecting the encoding:
// a JSON array (Management Activity API content blobs and most exports),
// newline-delimited or concatenated JSON objects, or CSV as produced by the
// Purview compliance portal.
func ReadStream(r io.Reader, fn RecordFunc) error {
	br := bufio.NewReaderSize(r, 1<<16)

	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		return ReadStream(gz, fn)
	}
	if bom, err := br.Peek(3); err == nil && bytes.Equal(bom, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}

	c, err := peekNonSpace(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty input is not an error
		}
		return err
	}

	switch c {
	case '[':
		return readJSONArray(br, fn)
	case '{':
		return readJSONObjects(br, fn)
	default:
		return readCSV(br, fn)
	}
}

// peekNonSpace consumes leading whitespace and returns the first meaningful
// byte without consuming it.
func peekNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.Peek(1)
		if err != nil {
			return 0, err
		}
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			if _, err := br.Discard(1); err != nil {
				return 0, err
			}
		default:
			return b[0], nil
		}
	}
}

func readJSONArray(r io.Reader, fn RecordFunc) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // opening '['
		return fmt.Errorf("json array: %w", err)
	}
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			return fmt.Errorf("json array: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	_, err := dec.Token() // closing ']'
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("json array: %w", err)
	}
	return nil
}

// readJSONObjects handles both newline-delimited JSON and objects
// concatenated without separators; encoding/json treats them identically.
func readJSONObjects(r io.Reader, fn RecordFunc) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	for {
		var rec map[string]any
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("json lines: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// readCSV parses a Purview audit export. Those files carry the real record
// as a JSON document in an AuditData column; the remaining columns are kept
// as a fallback when that column is absent.
func readCSV(r io.Reader, fn RecordFunc) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("csv header: %w", err)
	}
	columns := make([]string, len(header))
	auditDataCol := -1
	for i, name := range header {
		columns[i] = strings.TrimSpace(name)
		if strings.EqualFold(columns[i], "AuditData") {
			auditDataCol = i
		}
	}

	for {
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("csv: %w", err)
		}

		rec := make(map[string]any, len(columns))
		if auditDataCol >= 0 && auditDataCol < len(row) {
			if inner, ok := asObject(row[auditDataCol]); ok {
				rec = inner
			}
		}
		for i, value := range row {
			if i == auditDataCol || i >= len(columns) {
				continue
			}
			if value == "" {
				continue
			}
			if _, exists := rec[columns[i]]; !exists {
				rec[columns[i]] = value
			}
		}
		if len(rec) == 0 {
			continue
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}
