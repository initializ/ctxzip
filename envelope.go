package ctxzip

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/initializ/ctxzip/crush"
	"github.com/initializ/ctxzip/detect"
	"github.com/initializ/ctxzip/router"
)

// envelopeMinFieldChars is the size below which a field inside a JSON
// envelope is left alone — the marker overhead isn't worth it.
const envelopeMinFieldChars = 1024

// maxObjectDepth bounds the JSON-object recursion. Real traffic needs three
// levels: a runner envelope ({"stdout": ...}) whose string field holds a JSON
// object (kubectl -o json) whose array field ("items") holds the rows.
const maxObjectDepth = 3

// routeOne runs the standard detect → route → compress path on one blob.
func routeOne(r *router.Router, req crush.Request) (crush.Result, error) {
	det := detect.Detect(req.Content)
	return r.For(det.Type).Compress(req)
}

// routeAny compresses one blob, giving JSON objects structure-aware treatment
// (compressEnvelope) before falling back to the flat detect → route path.
func routeAny(r *router.Router, req crush.Request, depth int) (crush.Result, error) {
	if depth < maxObjectDepth {
		if res, ok := compressEnvelope(r, req, depth); ok {
			return res, nil
		}
	}
	return routeOne(r, req)
}

// compressEnvelope handles JSON-object shapes structure-aware, walking the
// object's top-level fields in order:
//
//   - large STRING fields are decoded back to real text (runner envelopes like
//     {"stdout": "<28 KB with \n escapes>", ...} have zero physical newlines,
//     defeating every detector — found live) and compressed recursively, so a
//     string that itself contains a JSON object gets object treatment too;
//   - large ARRAY fields are compressed as JSON arrays — kubectl -o json is
//     {"items": [...]}, and row selection keeps anomalous records WHOLE while
//     offloading the rest as retrievable complete records (found live: the
//     shape fell through to line-mode text dedup, scattering records);
//   - large OBJECT fields recurse the same way.
//
// Spliced non-string values must still be valid JSON or the field is left
// untouched; string values are re-quoted so any text is safe. Output remains
// a valid JSON object with untouched fields byte-identical and key order
// preserved, so the result is deterministic. Recursion is bounded by
// maxObjectDepth.
//
// Returns ok=false when the content is not a JSON object, nothing inside is
// worth compressing, or anything fails to parse — callers fall back to the
// direct routing path.
func compressEnvelope(r *router.Router, req crush.Request, depth int) (crush.Result, bool) {
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
		valueStart := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			// The envelope may have been truncated upstream (runtimes cap
			// tool output by size, cutting the JSON mid-string). If the cut
			// landed inside a STRING value — the overwhelmingly common case,
			// since the large text field dominates the envelope — salvage
			// the intact prefix instead of bailing to passthrough.
			return salvageTruncatedEnvelope(r, req, &b, first, key, trimmed[valueStart:])
		}

		val := []byte(raw)
		// Only large values are candidates; everything else is re-emitted
		// byte-identical.
		if len(raw) > envelopeMinFieldChars {
			switch raw[0] {
			case '"':
				// String field: decode to real text, compress recursively
				// (the text may itself be a JSON object — kubectl -o json
				// inside a runner envelope), re-quote. Quoting makes any
				// compressed text safe to splice.
				var inner string
				if json.Unmarshal(raw, &inner) == nil && len(inner) >= envelopeMinFieldChars {
					innerReq := req
					innerReq.Content = inner
					if cr, err := routeAny(r, innerReq, depth+1); err == nil &&
						cr.Compressed != inner && strings.TrimSpace(cr.Compressed) != "" {
						if enc, encErr := marshalJSONString(cr.Compressed); encErr == nil {
							val = enc
							changed = true
							markers = append(markers, cr.Markers...)
							strategy = cr.Strategy
						}
					}
				}
			case '[', '{':
				// Array or object field: compress the raw JSON text
				// recursively (arrays detect as JSONArray and get whole-row
				// selection; objects recurse here). Spliced UNQUOTED, so the
				// result must still be valid JSON — a fallback text-crush of
				// an object field would corrupt the parent and is rejected.
				innerReq := req
				innerReq.Content = string(raw)
				if cr, err := routeAny(r, innerReq, depth+1); err == nil &&
					cr.Compressed != string(raw) && json.Valid([]byte(cr.Compressed)) {
					val = []byte(cr.Compressed)
					changed = true
					markers = append(markers, cr.Markers...)
					strategy = cr.Strategy
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
	// Nested envelopes would otherwise stack prefixes
	// ("envelope:envelope:json_crusher"); keep one.
	strategy = strings.TrimPrefix(strategy, "envelope:")
	return crush.Result{
		Compressed: b.String(),
		Strategy:   "envelope:" + strategy,
		Markers:    markers,
	}, true
}

// truncSuffixRe matches the truncation notice runtimes append after cutting
// output at a size cap (forge's shape; kept anchored and specific).
var truncSuffixRe = regexp.MustCompile(`\n*\[OUTPUT TRUNCATED[^\]]*\]\s*$`)

// truncatedNote is added as an extra field on salvaged envelopes. It must be
// explicit that the missing tail was DESTROYED upstream, not offloaded —
// otherwise the model wastes a turn calling the expansion tool for bytes
// that do not exist.
const truncatedNote = "output was truncated upstream at the runtime's size cap before compression; " +
	"the tail beyond this point was destroyed, not offloaded — re-run the tool " +
	"(with a filter or pagination) if you need it"

// salvageTruncatedEnvelope recovers a JSON envelope whose serialization was
// cut mid-string by an upstream size cap. b holds the already-emitted
// complete fields; rawTail is everything from the failing value onward. Only
// the unambiguous case is salvaged — the tail begins a string value that
// never terminates; anything else bails to passthrough. The rebuilt envelope
// is valid JSON: complete fields byte-identical, the cut field's intact
// prefix compressed through the normal routing path, plus a "_ctxzip_note"
// field telling the model the tail is unrecoverable.
func salvageTruncatedEnvelope(r *router.Router, req crush.Request, b *strings.Builder, first bool, key, rawTail string) (crush.Result, bool) {
	// The tail starts right after the key token, so the "key: value"
	// separator is still in front of the value.
	rawTail = strings.TrimLeft(rawTail, " \t\r\n")
	rawTail = strings.TrimPrefix(rawTail, ":")
	rawTail = strings.TrimLeft(rawTail, " \t\r\n")
	if !strings.HasPrefix(rawTail, `"`) {
		return crush.Result{}, false // cut outside a string value — ambiguous, bail
	}
	// Strip the runtime's truncation notice (plain text appended after the
	// cut, textually inside the unterminated string) before unescaping.
	body := truncSuffixRe.ReplaceAllString(rawTail[1:], "")
	inner := bestEffortUnquote(body)
	if len(inner) < envelopeMinFieldChars {
		return crush.Result{}, false
	}

	innerReq := req
	innerReq.Content = inner
	cr, err := routeAny(r, innerReq, 1)
	if err != nil || strings.TrimSpace(cr.Compressed) == "" {
		return crush.Result{}, false
	}
	val, err := marshalJSONString(cr.Compressed)
	if err != nil {
		return crush.Result{}, false
	}
	keyBytes, err := json.Marshal(key)
	if err != nil {
		return crush.Result{}, false
	}
	noteBytes, err := marshalJSONString(truncatedNote)
	if err != nil {
		return crush.Result{}, false
	}

	if !first {
		b.WriteByte(',')
	}
	b.Write(keyBytes)
	b.WriteByte(':')
	b.Write(val)
	b.WriteString(`,"_ctxzip_note":`)
	b.Write(noteBytes)
	b.WriteByte('}')
	return crush.Result{
		Compressed: b.String(),
		Strategy:   "envelope_truncated:" + cr.Strategy,
		Markers:    cr.Markers,
	}, true
}

// bestEffortUnquote decodes the escaped body of a JSON string that has no
// closing quote (it was cut off), stopping cleanly at a trailing partial
// escape sequence. Surrogate pairs are decoded as individual code units —
// acceptable for salvaged text.
func bestEffortUnquote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '"' {
			break // terminated after all — take what precedes
		}
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			break // trailing lone backslash: the cut point
		}
		switch s[i+1] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '/':
			b.WriteByte('/')
		case 'b', 'f':
			// rare control escapes: drop the character, keep going
		case 'u':
			if i+6 <= len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 6
					continue
				}
			}
			return b.String() // malformed/partial \u at the cut: stop
		default:
			return b.String() // unknown escape at the cut: stop
		}
		i += 2
	}
	return b.String()
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
