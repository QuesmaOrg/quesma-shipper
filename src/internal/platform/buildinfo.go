package platform

// Package buildinfo answers "which build is this" from what the toolchain recorded. Tags do not
// reach this binary (see Info.Version), so a release name arrives as an ldflags stamp, honored
// only when the recorded revision corroborates it (see applyStamp).

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Info is what this binary knows about itself.
type Info struct {
	// Version is the module version when the toolchain derives one, and the commit otherwise: a subdirectory module never gets a tag.
	Version string

	// Revision is the full commit sha; Modified reports an uncommitted tree, whose code the sha alone cannot recover.
	Revision string
	Modified bool

	// Time is the commit time, not the build time: reproducible, and the question asked is which code this is.
	Time string

	GoVersion string
	OS        string
	Arch      string

	// Release reports a corroborated release stamp; it gates self-update, so a build that cannot prove which release it is stays put.
	Release bool
}

// releaseVersion is set by release CI via -ldflags -X; believed only when applyStamp corroborates it.
var releaseVersion string

var (
	once   sync.Once
	cached Info
)

// Current reads the stamped information once.
func Current() Info {
	once.Do(func() { cached = read() })
	return cached
}

func read() Info {
	i := Info{
		Version:   "unknown",
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		// Reported as unknown rather than invented: a made-up version in a manifest is worse than reporting unknown.
		return i
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		i.Version = bi.Main.Version
	}
	if bi.GoVersion != "" {
		i.GoVersion = bi.GoVersion
	}
	// A build inside a git worktree gets the primary checkout's HEAD and vcs.modified=false, so dev provenance there is best-effort.
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			i.Revision = s.Value
		case "vcs.time":
			i.Time = s.Value
		case "vcs.modified":
			i.Modified = s.Value == "true"
		case "GOOS":
			i.OS = s.Value
		case "GOARCH":
			i.Arch = s.Value
		}
	}
	// A checkout with no tags builds as "(devel)": naming it by its commit beats naming it unknown.
	if i.Version == "unknown" && i.Revision != "" {
		i.Version = "0.0.0-" + ShortRev(i.Revision)
		if i.Modified {
			i.Version += "+dirty"
		}
	}
	return applyStamp(i, releaseVersion)
}

// applyStamp honors a stamp only when the hash it ends with prefixes vcs.revision and the tree is clean.
func applyStamp(i Info, stamp string) Info {
	if stamp == "" {
		return i
	}
	dot := strings.LastIndex(stamp, ".")
	if dot < 0 || dot == len(stamp)-1 {
		return i
	}
	hash := stamp[dot+1:]
	if i.Revision == "" || !strings.HasPrefix(i.Revision, hash) || i.Modified {
		return i
	}
	i.Version = stamp
	i.Release = true
	return i
}

// String is the one form that goes everywhere: a version spelled two ways cannot be joined across an object and a trace.
func (i Info) String() string {
	v := i.Version
	if i.Modified && !strings.HasSuffix(v, "+dirty") {
		v += "+dirty"
	}
	return v
}

// Line is the human form, for `version` and for the root help.
func (i Info) Line() string {
	out := fmt.Sprintf("shipper %s %s/%s", i, i.OS, i.Arch)
	if i.Revision != "" {
		out += " commit=" + ShortRev(i.Revision)
	}
	if i.Time != "" {
		out += " committed=" + i.Time
	}
	return out + " " + i.GoVersion
}

// ShortRev is the 12-char commit spelling used everywhere a sha is shown.
func ShortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
