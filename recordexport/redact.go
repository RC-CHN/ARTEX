package recordexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

var secretLine = regexp.MustCompile(`(?im)((?:authorization|proxy-authorization|cookie|set-cookie|password|passwd|api[_-]?key|access[_-]?token|secret)\s*[:=]\s*)[^\r\n]+`)
var bearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)

func Redact(data []byte) []byte {
	return redactValue(data, 0)
}

func redactValue(data []byte, depth int) []byte {
	trimmed := bytes.TrimSpace(data)
	if depth < 32 && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) == nil && decoder.Decode(new(any)) == io.EOF {
			if encoded, err := json.Marshal(scrubAt(value, depth+1)); err == nil {
				return encoded
			}
		}
	}
	return bearer.ReplaceAll(secretLine.ReplaceAll(data, []byte("${1}[REDACTED]")), []byte("Bearer [REDACTED]"))
}
func scrub(v any) any {
	return scrubAt(v, 0)
}
func scrubAt(v any, depth int) any {
	switch value := v.(type) {
	case map[string]any:
		for k, item := range value {
			lower := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
			if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || lower == "authorization" || lower == "cookie" || lower == "set_cookie" || lower == "api_key" || lower == "access_token" {
				value[k] = "[REDACTED]"
			} else {
				value[k] = scrubAt(item, depth)
			}
		}
	case []any:
		for i, item := range value {
			value[i] = scrubAt(item, depth)
		}
	case string:
		return string(redactValue([]byte(value), depth))
	}
	return v
}

type redactJSONWriter struct {
	out     io.WriteCloser
	pending []byte
	err     error
}

// JSON rows remain valid JSON. Raw binary files are omitted separately.
func RedactJSON(out io.WriteCloser) io.WriteCloser { return &redactJSONWriter{out: out} }
func (w *redactJSONWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n := len(p)
	for len(p) > 0 {
		at := bytes.IndexByte(p, '\n')
		if at < 0 {
			w.pending = append(w.pending, p...)
			if len(w.pending) > 64<<20 {
				w.err = errors.New("redaction JSON row exceeds 64 MiB; use original export")
			}
			return n, w.err
		}
		w.pending = append(w.pending, p[:at]...)
		if len(w.pending) > 64<<20 {
			w.err = errors.New("redaction JSON row exceeds 64 MiB; use original export")
			return 0, w.err
		}
		if len(bytes.TrimSpace(w.pending)) > 0 {
			var v any
			decoder := json.NewDecoder(bytes.NewReader(w.pending))
			decoder.UseNumber()
			if w.err = decoder.Decode(&v); w.err != nil {
				return 0, w.err
			}
			w.err = json.NewEncoder(w.out).Encode(scrub(v))
			if w.err != nil {
				return 0, w.err
			}
		}
		w.pending = nil
		p = p[at+1:]
	}
	return n, nil
}
func (w *redactJSONWriter) Close() error {
	if len(bytes.TrimSpace(w.pending)) > 0 && w.err == nil {
		_, w.err = w.Write([]byte("\n"))
	}
	return errors.Join(w.err, w.out.Close())
}
