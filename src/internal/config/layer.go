package config

import "fmt"

// Layer is one step of the precedence chain; env vars are deliberately not one, so nothing the resolver enforces can be moved by one.
type Layer int

const (
	// LayerCompiledDefaults is the binary's own defaults: the scope ceiling. Not overridable.
	LayerCompiledDefaults Layer = iota + 1

	// LayerBundledCatalog is the embedded source-spec catalog: data, but compiled in, so it carries the ceiling.
	LayerBundledCatalog

	// LayerUser is the per-user config file, the highest layer a machine owner controls directly.
	LayerUser

	// LayerRemote is the org's served document, present only on an enrolled install. Optional everywhere.
	LayerRemote
)

// IsLocal reports whether a layer is under the machine owner's control: where a deny beats a remote allow.
func (l Layer) IsLocal() bool {
	switch l {
	case LayerCompiledDefaults, LayerBundledCatalog, LayerUser:
		return true
	default:
		return false
	}
}

func (l Layer) String() string {
	switch l {
	case LayerCompiledDefaults:
		return "compiled-defaults"
	case LayerBundledCatalog:
		return "bundled-catalog"
	case LayerUser:
		return "user"
	case LayerRemote:
		return "remote"
	default:
		return fmt.Sprintf("layer(%d)", int(l))
	}
}
