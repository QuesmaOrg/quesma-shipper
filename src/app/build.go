package app

// Build carries the binary's build identity.
type Build struct {
	// Version is DERIVED, not configured: app.NewBuild fills it from the binary's own build stamp.
	Version string

	// Release says the version above is a corroborated release stamp; it gates self-update, so a
	// build that cannot prove which release it is never decides it is out of date.
	Release bool
}
