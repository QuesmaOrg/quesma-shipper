package transforms

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// maxDepth bounds recursion in the JSON walk. Lines beyond it fall back to the raw
// scanner; 256 is 25 times deeper than the deepest observed transcript record.
const maxDepth = 256

var (
	errTooDeep         = fmt.Errorf("json nesting deeper than %d levels", maxDepth)
	errTrailingContent = errors.New("trailing content after JSON value")
	errStringToken     = errors.New("invalid JSON string token")
	errInvalidEdit     = errors.New("invalid JSON source edit")
	// An engine fault, not a parse failure: Scrub fails closed on it instead of raw-scanning.
	errEmbeddedEdit = errors.New("apply embedded JSON redaction spans")
)

var decoderOptions = []jsontext.Options{
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
}

// jsonWalker validates, decodes, and records source edits in one token pass. The
// bytes.Buffer lets jsontext borrow directly from the original line rather than copy
// it into the decoder's streaming buffer.
type jsonWalker struct {
	s      *Scrubber
	family string
	scan   *packs.ValueScan

	dec *jsontext.Decoder
	src bytes.Buffer

	edits []replacementSpan

	redacted int
	hits     map[string]int

	// Walks a string value that is itself a JSON document; one per nesting level, reused.
	inner *jsonWalker

	// Inside such a document no whole-value scan saw the numbers, so the walk scans them itself.
	embedded bool

	// Reused across values: an escape-dense Codex line would otherwise allocate each twice.
	unquoted, doc, rewritten []byte
}

func (w *jsonWalker) reset(s *Scrubber, family string, scan *packs.ValueScan, line []byte) {
	w.s = s
	w.family = family
	w.scan = scan
	w.edits = w.edits[:0]
	w.redacted = 0
	if w.hits == nil {
		w.hits = map[string]int{}
	} else {
		clear(w.hits)
	}

	w.src = *bytes.NewBuffer(line)
	if w.dec == nil {
		w.dec = jsontext.NewDecoder(&w.src, decoderOptions...)
	} else {
		w.dec.Reset(&w.src, decoderOptions...)
	}
}

func (w *jsonWalker) walkLine() error {
	if err := w.walk(0, "", ""); err != nil {
		return err
	}
	if _, err := w.dec.ReadToken(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errTrailingContent
}

func (w *jsonWalker) walk(depth int, key, path string) error {
	if depth > maxDepth {
		return errTooDeep
	}
	switch w.dec.PeekKind() {
	case '{':
		return w.walkObject(depth, path)
	case '[':
		return w.walkArray(depth, path)
	case '"':
		return w.walkString(depth, key, path)
	case '0':
		if w.embedded {
			return w.walkNumber(key, path)
		}
	}
	_, err := w.dec.ReadValue()
	return err
}

func (w *jsonWalker) walkObject(depth int, path string) error {
	if _, err := w.dec.ReadToken(); err != nil {
		return err
	}

	for w.dec.PeekKind() != '}' {
		raw, rawStart, key, err := w.readString()
		if err != nil {
			return err
		}

		field := joinFieldPath(path, key)
		plan := w.s.planValue(key, "", FieldPath(field), w.family, w.scan)
		w.addPlan(raw, rawStart, key, plan)
		if len(plan.spans) > 0 {
			field = joinFieldPath(path, plan.apply(key))
		}
		if err := w.walk(depth+1, key, field); err != nil {
			return err
		}
	}
	_, err := w.dec.ReadToken()
	return err
}

func (w *jsonWalker) walkArray(depth int, path string) error {
	if _, err := w.dec.ReadToken(); err != nil {
		return err
	}
	path += "[]"

	for w.dec.PeekKind() != ']' {
		if err := w.walk(depth+1, "", path); err != nil {
			return err
		}
	}
	_, err := w.dec.ReadToken()
	return err
}

func (w *jsonWalker) walkString(depth int, key, path string) error {
	raw, rawStart, text, err := w.readString()
	if err != nil {
		return err
	}
	// A document the inner walk completes on is scanned leaf by leaf, so the whole-value scan
	// would read every byte twice; a secret-named key or a key-plus-value rule hit still takes it.
	if looksLikeDocument(text) && !w.s.keyNamesSecret(key, text) && !w.s.spansKeyAndValue(text) {
		embedded, walked, err := w.walkEmbedded(depth, path, text)
		if err != nil {
			return err
		}
		if walked {
			if embedded != text {
				w.edits = append(w.edits, replacementSpan{Start: rawStart, End: rawStart + len(raw), Replacement: embedded})
			}
			return nil
		}
	}
	plan := w.s.planValue(text, key, FieldPath(path), w.family, w.scan)
	if len(plan.spans) == 0 {
		return nil
	}
	embedded, _, err := w.walkEmbedded(depth, path, plan.apply(text))
	if err != nil {
		return err
	}
	w.edits = append(w.edits, replacementSpan{Start: rawStart, End: rawStart + len(raw), Replacement: embedded})
	w.addLedger(plan)
	return nil
}

// walkNumber gives a number the ladder a string gets; a redacted one becomes a JSON string.
func (w *jsonWalker) walkNumber(key, path string) error {
	raw, err := w.dec.ReadValue()
	if err != nil {
		return err
	}
	text := string(raw)
	plan := w.s.planValue(text, key, FieldPath(path), w.family, w.scan)
	w.addPlan(raw, int(w.dec.InputOffset())-len(raw), text, plan)
	return nil
}

// embeddedSuffix marks a path as descending into a string that holds a JSON document, so an
// exemption for the outer field never reaches the fields encoded inside it.
const embeddedSuffix = "#json"

// walkEmbedded walks a string value that is a JSON object or array (tool arguments and outputs
// stored as encoded JSON) so its keys get key-name redaction and its leaves the full ladder. A
// value that is not JSON comes back unchanged with walked false.
func (w *jsonWalker) walkEmbedded(depth int, path, text string) (out string, walked bool, err error) {
	if !looksLikeDocument(text) {
		return text, false, nil
	}
	if w.inner == nil {
		w.inner = &jsonWalker{embedded: true}
	}
	in := w.inner
	in.doc = append(in.doc[:0], text...)
	in.reset(w.s, w.family, w.scan, in.doc)
	if err := in.walk(depth+1, "", path+embeddedSuffix); errors.Is(err, errEmbeddedEdit) {
		return "", false, err
	} else if err != nil {
		return text, false, nil
	}
	if _, err := in.dec.ReadToken(); !errors.Is(err, io.EOF) {
		return text, false, nil
	}
	if len(in.edits) == 0 {
		return text, true, nil
	}
	rewritten, err := in.appendTo(in.rewritten[:0], in.doc)
	if err != nil {
		return "", false, fmt.Errorf("%w at %s: %w", errEmbeddedEdit, path, err)
	}
	in.rewritten = rewritten
	w.redacted += in.redacted
	for id, count := range in.hits {
		w.hits[id] += count
	}
	return string(rewritten), true, nil
}

func looksLikeDocument(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	return trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[')
}

func (w *jsonWalker) readString() (raw []byte, rawStart int, text string, err error) {
	raw, err = w.dec.ReadValue()
	if err != nil {
		return nil, 0, "", err
	}
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, 0, "", errStringToken
	}
	body := raw[1 : len(raw)-1]
	if bytes.IndexByte(body, '\\') < 0 && utf8.Valid(body) {
		return raw, int(w.dec.InputOffset()) - len(raw), string(body), nil
	}
	w.unquoted, _ = jsontext.AppendUnquote(w.unquoted[:0], raw)
	return raw, int(w.dec.InputOffset()) - len(raw), string(w.unquoted), nil
}

func joinFieldPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func (w *jsonWalker) addPlan(raw []byte, rawStart int, decoded string, plan valuePlan) {
	if len(plan.spans) == 0 {
		return
	}
	w.edits = append(w.edits, replacementSpan{
		Start: rawStart, End: rawStart + len(raw), Replacement: plan.apply(decoded),
	})
	w.addLedger(plan)
}

func (w *jsonWalker) addLedger(plan valuePlan) {
	w.redacted += plan.redacted
	for id, count := range plan.hits {
		w.hits[id] += count
	}
}

func (w *jsonWalker) appendTo(out, line []byte) ([]byte, error) {
	cursor := 0
	for _, edit := range w.edits {
		if edit.Start < cursor || edit.End < edit.Start || edit.End > len(line) {
			return nil, errInvalidEdit
		}
		out = append(out, line[cursor:edit.Start]...)
		var err error
		out, err = jsontext.AppendQuote(out, edit.Replacement)
		if err != nil {
			return nil, err
		}
		cursor = edit.End
	}
	return append(out, line[cursor:]...), nil
}

func nextLine(p []byte) (body, ending, rest []byte) {
	idx := bytes.IndexByte(p, '\n')
	if idx < 0 {
		return p, nil, nil
	}
	body = p[:idx]
	ending = p[idx : idx+1]
	if len(body) > 0 && body[len(body)-1] == '\r' {
		body = body[:len(body)-1]
		ending = p[idx-1 : idx+1]
	}
	return body, ending, p[idx+1:]
}
