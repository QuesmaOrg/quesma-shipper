package sqliteread

import (
	"encoding/json"
	"strings"
)

// The compiled key/field deny, not configurable at any layer. Auth material lives inside the SAME database as
// the trajectories, and a path deny list cannot express "this file, minus these rows", so the exclusion has to
// happen at the row and field level, inside the read itself.

// deniedKeyPrefixes are keyspaces no enricher may read: cursorAuth/* holds Cursor's own session tokens.
var deniedKeyPrefixes = []string{
	"cursorauth/",
	"cursorauth.",
}

// deniedFields are JSON field names stripped from every value at any depth: Cursor's blob keys, stored beside the content they protect.
var deniedFields = []string{
	"blobEncryptionKey",
	"speculativeSummarizationEncryptionKey",
}

// allowedExactKeys are the compiled exceptions to the prefix deny. EXACT keys only: a prefix would ship the next key Cursor adds.
var allowedExactKeys = map[string]bool{
	"cursorauth/stripemembershiptype": true, // the plan (free/pro/business/enterprise)
	"cursorauth/cachedemail":          true, // which account the plan belongs to
	"cursorauth/cachedsignuptype":     true, // how the account authenticates (Google, ...)
	"cursorauth/cachedteam":           true, // {teamId, name}, no credential material
}

// keyDenied reports whether a row key is off limits. Case-insensitive: the namespace is spelled more than one way.
func keyDenied(key string) bool {
	lower := strings.ToLower(key)
	if allowedExactKeys[lower] {
		return false
	}
	for _, p := range deniedKeyPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// scrubValue removes denied fields at any depth; a value that is not JSON has no fields to strip and is returned unchanged.
func scrubValue(raw []byte) ([]byte, int) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw, 0
	}
	cleaned, n := stripFields(v)
	if n == 0 {
		// Returned verbatim when nothing was stripped: re-marshalling reorders keys, which would make the output depend on map iteration.
		return raw, 0
	}
	out, err := json.Marshal(cleaned)
	if err != nil {
		// Unreachable in practice; dropping the row is still the only outcome that cannot leak the field.
		return nil, n
	}
	return out, n
}

func stripFields(v any) (any, int) {
	switch t := v.(type) {
	case map[string]any:
		removed := 0
		for _, f := range deniedFields {
			if _, ok := t[f]; ok {
				delete(t, f)
				removed++
			}
		}
		for k, child := range t {
			c, n := stripFields(child)
			t[k] = c
			removed += n
		}
		return t, removed
	case []any:
		removed := 0
		for i, child := range t {
			c, n := stripFields(child)
			t[i] = c
			removed += n
		}
		return t, removed
	default:
		return v, 0
	}
}
