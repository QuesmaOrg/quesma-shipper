package engine

import (
	"os/user"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// UsernameFromStateDir derives the OS user name for the path placeholder, from the resolved state
// directory where possible so redaction and the canonical object key use the same value. Exported
// so doctor previews with the same derivation a real run uses.
func UsernameFromStateDir(stateDir string) string {
	if stateDir != "" {
		if name := formats.UsernameFromPath(stateDir); name != "" {
			return name
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
