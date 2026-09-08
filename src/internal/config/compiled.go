// Package config resolves the configuration layers into one effective configuration and records
// where every value came from. Compiled defaults and the bundled catalog carry the ceiling, the
// user file and the served document configure within it, and on widening the local layer wins.
//
// Glob narrowness is deliberately not compared: what stops a widened glob is the
// compiled deny list on symlink-resolved paths and require_subdir on the root's shape.
package config

// AcceptedConfigVersions is enumerated, never a range: an unknown config_version is a hard error.
var AcceptedConfigVersions = []int{1}
