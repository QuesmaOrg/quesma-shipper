package transforms

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
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

	// path is the dotted field path of the value being walked, built only when some field
	// is exempt for this family.
	path      []byte
	trackPath bool

	redacted int
	hits     map[string]int
}

func (w *jsonWalker) reset(s *Scrubber, family string, scan *packs.ValueScan, line []byte) {
	w.s = s
	w.family = family
	w.scan = scan
	w.edits = w.edits[:0]
	w.path = w.path[:0]
	w.trackPath = !s.exempt.None(family)
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
	if err := w.walk(0, ""); err != nil {
		return err
	}
	if _, err := w.dec.ReadToken(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errTrailingContent
}

func (w *jsonWalker) walk(depth int, key string) error {
	if depth > maxDepth {
		return errTooDeep
	}
	switch w.dec.PeekKind() {
	case '{':
		return w.walkObject(depth)
	case '[':
		return w.walkArray(depth)
	case '"':
		return w.walkString(key)
	default:
		_, err := w.dec.ReadValue()
		return err
	}
}

func (w *jsonWalker) walkObject(depth int) error {
	if _, err := w.dec.ReadToken(); err != nil {
		return err
	}
	parent := len(w.path)

	for w.dec.PeekKind() != '}' {
		raw, rawStart, key, err := w.readString()
		if err != nil {
			return err
		}

		w.setField(parent, key)
		plan := w.s.planValue(key, "", w.exempt(), w.scan)
		w.addPlan(raw, rawStart, key, plan)
		if len(plan.spans) > 0 && w.trackPath {
			w.setField(parent, plan.apply(key))
		}
		if err := w.walk(depth+1, key); err != nil {
			return err
		}
	}
	w.path = w.path[:parent]
	_, err := w.dec.ReadToken()
	return err
}

func (w *jsonWalker) walkArray(depth int) error {
	if _, err := w.dec.ReadToken(); err != nil {
		return err
	}
	parent := len(w.path)
	if w.trackPath {
		w.path = append(w.path, "[]"...)
	}

	for w.dec.PeekKind() != ']' {
		if err := w.walk(depth+1, ""); err != nil {
			return err
		}
	}
	w.path = w.path[:parent]
	_, err := w.dec.ReadToken()
	return err
}

func (w *jsonWalker) walkString(key string) error {
	raw, rawStart, text, err := w.readString()
	if err != nil {
		return err
	}
	w.addPlan(raw, rawStart, text, w.s.planValue(text, key, w.exempt(), w.scan))
	return nil
}

// setField replaces everything after the first parent bytes of path with child. An empty
// parent adds no dot, even under an empty key: {"":{"b":…}} walks b as "b".
func (w *jsonWalker) setField(parent int, child string) {
	if !w.trackPath {
		return
	}
	w.path = w.path[:parent]
	if parent > 0 {
		w.path = append(w.path, '.')
	}
	w.path = append(w.path, child...)
}

func (w *jsonWalker) exempt() bool {
	return w.trackPath && w.s.exempt.Exempt(w.family, w.path)
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
	decoded, _ := jsontext.AppendUnquote(nil, raw)
	return raw, int(w.dec.InputOffset()) - len(raw), string(decoded), nil
}

func (w *jsonWalker) addPlan(raw []byte, rawStart int, decoded string, plan valuePlan) {
	if len(plan.spans) == 0 {
		return
	}
	w.edits = append(w.edits, replacementSpan{
		Start: rawStart, End: rawStart + len(raw), Replacement: plan.apply(decoded),
	})
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
