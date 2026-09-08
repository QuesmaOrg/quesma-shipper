package common

import "testing"

func TestNewer(t *testing.T) {
	const latest = "0.0.1-124.def456def456"

	for _, tc := range []struct {
		name    string
		current string
		want    bool
	}{
		{"older release", "0.0.1-123.abcdef123456", true},
		{"same release", latest, false},
		{"newer release", "0.0.1-125.aaaaaaaaaaaa", false},
		{"numeric commit count", "0.0.1-99.bbb222bbb222", true},
		{"development build", "0.0.0-031a7faa8c16", true},
		{"unknown build", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newer(latest, tc.current); got != tc.want {
				t.Errorf("newer(%q, %q) = %v, want %v", latest, tc.current, got, tc.want)
			}
		})
	}
}

func TestNewReleaseLineSupersedesOldRepository(t *testing.T) {
	const (
		latest  = "0.0.2-1.abcdef123456"
		current = "0.0.1-283.ad5a94045497"
	)
	if !newer(latest, current) {
		t.Fatalf("newer(%q, %q) = false, want true", latest, current)
	}
}
