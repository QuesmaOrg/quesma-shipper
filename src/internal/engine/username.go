package engine

import (
	"os/user"
	"strings"
)

// UsernameFromStateDir derives the OS user name for the path placeholder, from the resolved state
// directory where possible so redaction and the canonical object key use the same value. Exported
// so doctor previews with the same derivation a real run uses.
func UsernameFromStateDir(stateDir string) string {
	// A home-shaped path names the user: /Users/<name> on macOS, /home/<name> on Linux.
	segments := strings.Split(strings.ReplaceAll(stateDir, `\`, "/"), "/")
	for i, seg := range segments {
		if (seg == "Users" || seg == "home") && i+1 < len(segments) {
			return segments[i+1]
		}
	}
	// Windows usernames come as HOST\name; we only care about the name.
	if u, err := user.Current(); err == nil {
		if _, name, ok := strings.Cut(u.Username, `\`); ok {
			return name
		}
		return u.Username
	}
	return ""
}
