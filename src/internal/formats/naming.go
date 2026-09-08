package formats

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// NameKeySize is the per-install secret's length in bytes, sized for HMAC-SHA256.
const NameKeySize = 32

// mirrorDomain tags the HMAC input so a later name kind cannot collide with mirror keys.
const mirrorDomain = "mirror:"

// UserPlaceholder replaces the OS user's name inside a canonical path. For convergence, not
// concealment: the same logical file keeps its key when the home path changes underneath it.
const UserPlaceholder = "__USER__"

// identifierRe is the grammar for organization, install and source ids. Enforced because a
// value containing "/" or "=" is interpolated into an object key and could forge a path.
var identifierRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// CanonicalPath is the deterministic string a mirror name is derived from: identical on every run
// and every machine of the identity unit, so a re-ship after state loss overwrites, never duplicates.
func CanonicalPath(sourceRelPath, username string) string {
	p := path.Clean(strings.ReplaceAll(sourceRelPath, `\`, "/"))
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		p = ""
	}
	p = ApplyUserPlaceholder(p, username)

	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = encodeSegment(s)
	}
	return strings.Join(segs, "/")
}

// ApplyUserPlaceholder replaces occurrences of username bounded by the string edges or by a
// non-alphanumeric, so "janes-notes.md" survives. A name under two characters is ignored.
func ApplyUserPlaceholder(s, username string) string {
	if len(username) < 2 || s == "" {
		return s
	}

	var b strings.Builder
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], username) {
			b.WriteByte(s[i])
			i++
			continue
		}
		leftOK := i == 0 || !isAlnum(s[i-1])
		end := i + len(username)
		rightOK := end == len(s) || !isAlnum(s[end])
		if leftOK && rightOK {
			b.WriteString(UserPlaceholder)
			i = end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// encodeSegment percent-encodes everything outside [A-Za-z0-9._~-], "%" included, so the
// encoding is injective. Upper-case hex keeps the output byte-identical everywhere.
func encodeSegment(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlnum(c), c == '.', c == '_', c == '~', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// MirrorName derives an object's leaf name: HMAC-SHA256(name_key, "mirror:" + canonical path).
// Keyed, because a plaintext path hash in a listable key is a confirmation oracle. Path-derived
// and never content-derived, so a file's whole history lands on one key.
func MirrorName(nameKey []byte, canonicalPath string) string {
	m := hmac.New(sha256.New, nameKey)
	m.Write([]byte(mirrorDomain))
	m.Write([]byte(canonicalPath))
	return hex.EncodeToString(m.Sum(nil))
}

// MirrorKey composes the full object key, identity-first (layout version, organization, install,
// container) so erasure is one prefix delete. Standalone installs pass "default" as the org.
func MirrorKey(org, installID, sourceID string, nameKey []byte, canonicalPath string) (string, error) {
	root, err := InstallRoot(org, installID)
	if err != nil {
		return "", err
	}
	if err := checkSegment("source", sourceID); err != nil {
		return "", err
	}
	if len(nameKey) != NameKeySize {
		return "", fmt.Errorf("identity: name_key is %d bytes, want %d", len(nameKey), NameKeySize)
	}
	return fmt.Sprintf("%s/mirror/source=%s/%s.age",
		root, sourceID, MirrorName(nameKey, canonicalPath)), nil
}

// StateKey composes the key of an install-owned state object, under the same install prefix
// so one sweep erases it with everything else.
func StateKey(org, installID, name string) (string, error) {
	root, err := InstallRoot(org, installID)
	if err != nil {
		return "", err
	}
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("identity: state object name %q must be non-empty and contain no slash", name)
	}
	return root + "/state/" + name, nil
}

func checkSegment(kind, v string) error {
	if !identifierRe.MatchString(v) {
		return fmt.Errorf("identity: %s segment %q is not a legal key segment", kind, v)
	}
	return nil
}

// UsernameFromPath pulls the OS user name out of a home-shaped path: /Users/<name> on macOS,
// /home/<name> on Linux. It lives here so engine and doctor cannot disagree about it.
func UsernameFromPath(p string) string {
	segments := strings.Split(strings.ReplaceAll(p, `\`, "/"), "/")
	for i, seg := range segments {
		if (seg == "Users" || seg == "home") && i+1 < len(segments) {
			return segments[i+1]
		}
	}
	return ""
}

// InstallRoot is the key prefix an install owns, composed from the same segments MirrorKey and
// StateKey use so the server-side key scope cannot drift from the keys actually written.
func InstallRoot(org, installID string) (string, error) {
	if err := checkSegment("organization", org); err != nil {
		return "", err
	}
	if err := checkSegment("install", installID); err != nil {
		return "", err
	}
	return fmt.Sprintf("v1/organization=%s/install=%s", org, installID), nil
}
