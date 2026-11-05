package sources

import (
	"cmp"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// gitConfigMaxBytes bounds the read of a repository's config; a real one is a few kilobytes.
const gitConfigMaxBytes = 256 << 10

// gitRemotes reads only remote.<name>.url out of a repository's config: the file can also hold
// credentials (http.extraHeader), so nothing else is taken, and an unreadable config is no remotes.
func gitRemotes(commonDir string) []formats.Remote {
	body, _, err := platform.ReadWhole(filepath.Join(commonDir, "config"), gitConfigMaxBytes)
	if err != nil {
		return nil
	}
	var out []formats.Remote
	remote := ""
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			remote = remoteSection(line)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if remote == "" || !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if host, path := normaliseRemote(configValue(value)); host != "" {
			if r := (formats.Remote{Name: remote, Host: host, Path: path}); !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	slices.SortFunc(out, func(a, b formats.Remote) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Host, b.Host), strings.Compare(a.Path, b.Path))
	})
	return out
}

// remoteSection is the name in [remote "name"] or the older [remote.name], "" for any other section.
func remoteSection(header string) string {
	inner, _, _ := strings.Cut(strings.TrimPrefix(header, "["), "]")
	inner = strings.TrimSpace(inner)
	if name, ok := strings.CutPrefix(inner, "remote "); ok {
		return strings.Trim(strings.TrimSpace(name), `"`)
	}
	if name, ok := strings.CutPrefix(strings.ToLower(inner), "remote."); ok {
		return name
	}
	return ""
}

// configValue drops a quoted value's quotes and an unquoted value's trailing comment.
func configValue(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, `"`); ok {
		v, _, _ = strings.Cut(rest, `"`)
		return v
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// normaliseRemote is a remote's host and path whatever the transport, so https, ssh, git and
// scp-like user@host:path name a repository alike. A local path or file:// remote names no host,
// so both are "".
func normaliseRemote(raw string) (host, path string) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i > 0 {
		u, err := url.Parse(s)
		if err != nil || strings.EqualFold(u.Scheme, "file") {
			return "", ""
		}
		host, path = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	} else if i := strings.Index(s, ":"); i > 1 && !strings.ContainsAny(s[:i], `/\`) {
		// A single letter before the colon is a Windows drive, not a host.
		host, path = s[:i], s[i+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	} else {
		return "", ""
	}
	host = strings.ToLower(host)
	path = strings.TrimRight(strings.TrimSuffix(strings.TrimRight(path, "/"), ".git"), "/")
	if host == "" || path == "" {
		return "", ""
	}
	return host, path
}
