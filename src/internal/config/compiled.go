// Package config resolves configuration layers and records where each value came from.
// The control plane configures collection scope; local disables and the deny list remain authoritative.
package config

// AcceptedConfigVersions is enumerated, never a range: an unknown config_version is a hard error.
var AcceptedConfigVersions = []int{1}
