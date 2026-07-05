package ctxzip

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/initializ/ctxzip/crush"
	"github.com/initializ/ctxzip/detect"
	"github.com/initializ/ctxzip/router"
)

// envelopeMinFieldChars is the size below which a string field inside a JSON
// envelope is left alone — the marker overhead isn't worth it.
const envelopeMinFieldChars = 1024

// routeOne runs the standard detect → route → compress path on one blob.
func routeOne(r *router.Router, req crush.Request) (crush.Result, error) {
	det := detect.Detect(req.Content)
	return r.For(det.Type).Compress(req)
}

// compressEnvelope handles tool outputs wrapped in a single-line JSON object
// envelope — the shape CLI/tool runners commonly produce:
//
//	{"stdout": "<28 KB of tabular text with \n escapes>", "stderr": "", "exit_code": 0}
//
// Serialized this way, the payload has zero physical newlines and no
// detectable structure, so every content-aware crusher passes it through
// (found live: kubectl output through forge's cli_execute never compressed).
// This walks the object's top-level fields in order, decompresses each large
// string field back to real text, compresses THAT with the normal routing
// path, and splices the compressed value back — output remains a valid JSON
// object with untouched fields byte-identical and key order preserved, so the
// result is deterministic. Depth is deliberately one level: nested envelopes
// haven't been observed, and recursion without a cycle guard invites trouble.
//
// Returns ok=false when the content is not a JSON object, nothing inside is
// worth compressing, or anything fails to parse — callers fall back to the
// direct routing path.
func compressEnvelope(r *router.Router, req crush.Request) (crush.Result, bool) {
	trimmed := strings.TrimSpace(req.Content)
	if !strings.HasPrefix(trimmed, "{") {
		return crush.Result{}, false
	}

	dec := json.NewDecoder(strings.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return crush.Result{}, false
	}

	var b strings.Builder
	b.WriteByte('{')
	first := true
	changed := false
	var markers []string
	var strategy string

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return crush.Result{}, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return crush.Result{}, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return crush.Result{}, false
		}

		val := []byte(raw)
		// Only large string values are candidates; everything else is
		// re-emitted byte-identical.
		if len(raw) > envelopeMinFieldChars && raw[0] == '"' {
			var inner string
			if json.Unmarshal(raw, &inner) == nil && len(inner) >= envelopeMinFieldChars {
				innerReq := req
				innerReq.Content = inner
				if cr, err := routeOne(r, innerReq); err == nil &&
					cr.Compressed != inner && strings.TrimSpace(cr.Compressed) != "" {
					if enc, encErr := marshalJSONString(cr.Compressed); encErr == nil {
						val = enc
						changed = true
						markers = append(markers, cr.Markers...)
						strategy = cr.Strategy
					}
				}
			}
		}

		if !first {
			b.WriteByte(',')
		}
		first = false
		keyBytes, err := json.Marshal(key)
		if err != nil {
			return crush.Result{}, false
		}
		b.Write(keyBytes)
		b.WriteByte(':')
		b.Write(val)
	}

	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return crush.Result{}, false
	}
	if _, err := dec.Token(); err == nil {
		// Trailing content after the object — not a pure envelope.
		return crush.Result{}, false
	}
	if !changed {
		return crush.Result{}, false
	}

	b.WriteByte('}')
	return crush.Result{
		Compressed: b.String(),
		Strategy:   "envelope:" + strategy,
		Markers:    markers,
	}, true
}

// marshalJSONString encodes s as a JSON string without HTML escaping, so the
// "<<ctxzip:...>>" marker inside stays literal for the model to read.
func marshalJSONString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
