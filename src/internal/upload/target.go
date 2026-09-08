// Package upload sends one prepared object through one presigned PUT ticket. The bytes go to a
// URL the control plane issued, so local validation is the only defence against a URL naming
// another install's key. A ticket URL is a credential: no log line, span or error may carry one.
package upload

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Addressing is the URL layout a target uses; an unknown form is refused rather than guessed at.
type Addressing string

const (
	// VirtualHosted puts the bucket in the host: the path is "/" plus the object key.
	VirtualHosted Addressing = "virtual-hosted"
	// PathStyle puts the bucket in the path: the path is the pinned prefix plus the key.
	PathStyle Addressing = "path-style"

	// addressingUnpinned marks a target adopted from a ticket's own origin when nothing was
	// pinned, so the exact-key check accepts either layout. unpinnedTarget is its only source.
	addressingUnpinned Addressing = "unpinned"
)

// ErrNoTarget names the origin and never the path or query: the origin is not the secret part.
var ErrNoTarget = errors.New("upload: ticket origin is not an allowed upload target")

// TargetSpec is the machine owner's declaration of one upload target: local configuration or
// enrollment only, never an authorization response, and served configuration may not write it.
type TargetSpec struct {
	// Origin is scheme://host[:port] and nothing else: no path, query, fragment or userinfo.
	Origin     string
	Addressing Addressing
	// PathPrefix is empty for virtual-hosted and "/bucket" for path-style, compared literally.
	PathPrefix string
	// AllowLoopbackHTTP admits http:// for a loopback host. Development only.
	AllowLoopbackHTTP bool
}

// UploadTarget is one validated origin plus its addressing; the zero value matches nothing.
type UploadTarget struct {
	scheme     string
	host       string
	port       string // always explicit, so :443 and the default form compare equal
	addressing Addressing
	pathPrefix string
}

// UploadTargetList is the machine owner's optional origin pin; empty means unpinned (see Match).
// A list, so a store migration can admit two explicit origins without wildcard matching.
type UploadTargetList []UploadTarget

// NewUploadTarget validates one machine-owner declaration.
func NewUploadTarget(spec TargetSpec) (UploadTarget, error) {
	u, err := url.Parse(strings.TrimSpace(spec.Origin))
	if err != nil {
		return UploadTarget{}, fmt.Errorf("upload: target origin %q does not parse", spec.Origin)
	}
	switch {
	case u.Opaque != "":
		return UploadTarget{}, fmt.Errorf("upload: target origin %q is opaque", spec.Origin)
	case u.Host == "":
		return UploadTarget{}, fmt.Errorf("upload: target origin %q names no host", spec.Origin)
	case u.User != nil:
		return UploadTarget{}, fmt.Errorf("upload: target origin %q carries user information", spec.Origin)
	case u.Fragment != "":
		return UploadTarget{}, fmt.Errorf("upload: target origin %q carries a fragment", spec.Origin)
	case u.RawQuery != "":
		return UploadTarget{}, fmt.Errorf("upload: target origin %q carries a query", spec.Origin)
	case u.Path != "" && u.Path != "/":
		return UploadTarget{}, fmt.Errorf("upload: target origin %q carries a path: put the bucket in PathPrefix", spec.Origin)
	}

	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "*") {
		return UploadTarget{}, fmt.Errorf("upload: target origin %q is a wildcard host", spec.Origin)
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !spec.AllowLoopbackHTTP {
			return UploadTarget{}, fmt.Errorf("upload: target origin %q is http and loopback development mode is off", spec.Origin)
		}
		if !isLoopbackHost(host) {
			return UploadTarget{}, fmt.Errorf("upload: target origin %q is http but %q is not loopback", spec.Origin, host)
		}
	default:
		return UploadTarget{}, fmt.Errorf("upload: target origin %q uses scheme %q", spec.Origin, u.Scheme)
	}

	port := effectivePort(u)

	prefix, err := validatePathPrefix(spec.Addressing, spec.PathPrefix)
	if err != nil {
		return UploadTarget{}, err
	}
	return UploadTarget{
		scheme:     scheme,
		host:       host,
		port:       port,
		addressing: spec.Addressing,
		pathPrefix: prefix,
	}, nil
}

// Origin is the pinned scheme://host:port, with the port always explicit.
func (t UploadTarget) Origin() string {
	return t.scheme + "://" + net.JoinHostPort(t.host, t.port)
}

// Match returns the entry a ticket URL belongs to, parsing the URL here so no caller outside
// this package holds one. An empty list is unpinned: the ticket's own origin becomes the target,
// but every other check still applies, so an owner trusts the plane on WHERE, never on WHAT.
func (l UploadTargetList) Match(rawURL string) (UploadTarget, error) {
	u, err := parseTicketURL(rawURL)
	if err != nil {
		return UploadTarget{}, err
	}
	if len(l) == 0 {
		return unpinnedTarget(u)
	}
	for _, t := range l {
		if t.matchesOrigin(u) {
			return t, nil
		}
	}
	return UploadTarget{}, fmt.Errorf("%w: %s", ErrNoTarget, originOf(u))
}

// unpinnedTarget adopts a ticket's own origin, https only: with no pinned entry there is no
// opt-in to read, and a presigned URL in cleartext is a credential exposed on the wire.
func unpinnedTarget(u *url.URL) (UploadTarget, error) {
	if u.Scheme != "https" {
		return UploadTarget{}, errors.New(
			"upload: ticket url is http and no upload targets are configured: only a configured " +
				"upload_targets entry may admit a non-https origin")
	}
	return UploadTarget{
		scheme:     "https",
		host:       strings.ToLower(u.Hostname()),
		port:       effectivePort(u),
		addressing: addressingUnpinned,
	}, nil
}

func (t UploadTarget) matchesOrigin(u *url.URL) bool { return t.Origin() == originOf(u) }

// parseTicketURL is the single door a ticket URL enters through; its errors quote nothing.
func parseTicketURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("upload: ticket url does not parse")
	}
	switch {
	case u.Opaque != "":
		return nil, errors.New("upload: ticket url is opaque")
	case u.Host == "":
		return nil, errors.New("upload: ticket url names no host")
	case u.User != nil:
		return nil, errors.New("upload: ticket url carries user information")
	case u.Fragment != "", u.RawFragment != "":
		return nil, errors.New("upload: ticket url carries a fragment")
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, fmt.Errorf("upload: ticket url uses scheme %q", u.Scheme)
	}
	return u, nil
}

func originOf(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), effectivePort(u))
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return "443"
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validatePathPrefix keeps the bucket where the addressing says; escapes would make the
// exact-key comparison ambiguous, so a prefix must be literal.
func validatePathPrefix(addressing Addressing, prefix string) (string, error) {
	switch addressing {
	case VirtualHosted:
		if prefix != "" {
			return "", fmt.Errorf("upload: virtual-hosted target declares path prefix %q: the bucket is already the host", prefix)
		}
		return "", nil
	case PathStyle:
		if !strings.HasPrefix(prefix, "/") || prefix == "/" {
			return "", fmt.Errorf("upload: path-style target needs a /bucket path prefix, got %q", prefix)
		}
		if strings.HasSuffix(prefix, "/") {
			return "", fmt.Errorf("upload: path prefix %q ends in a slash", prefix)
		}
		body := strings.TrimPrefix(prefix, "/")
		if err := validateSegments(body); err != nil {
			return "", fmt.Errorf("upload: path prefix %q: %w", prefix, err)
		}
		if canonicalPath(body) != body {
			return "", fmt.Errorf("upload: path prefix %q is not literal", prefix)
		}
		return prefix, nil
	default:
		return "", fmt.Errorf("upload: unknown addressing form %q", string(addressing))
	}
}
