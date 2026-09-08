package upload

import (
	"errors"
	"strings"
	"testing"
)

func TestNewUploadTargetAccepts(t *testing.T) {
	cases := []struct {
		name       string
		spec       TargetSpec
		wantOrigin string
	}{
		{
			name:       "https default port",
			spec:       TargetSpec{Origin: "https://archive.example.invalid", Addressing: VirtualHosted},
			wantOrigin: "https://archive.example.invalid:443",
		},
		{
			name:       "explicit port matches the default",
			spec:       TargetSpec{Origin: "https://archive.example.invalid:443", Addressing: VirtualHosted},
			wantOrigin: "https://archive.example.invalid:443",
		},
		{
			name:       "mixed case host normalizes",
			spec:       TargetSpec{Origin: "https://Archive.Example.Invalid", Addressing: VirtualHosted},
			wantOrigin: "https://archive.example.invalid:443",
		},
		{
			name:       "path-style pins the bucket prefix",
			spec:       TargetSpec{Origin: "https://minio.example.invalid:9000", Addressing: PathStyle, PathPrefix: "/trajectories"},
			wantOrigin: "https://minio.example.invalid:9000",
		},
		{
			name:       "loopback http with the development flag",
			spec:       TargetSpec{Origin: "http://127.0.0.1:9000", Addressing: PathStyle, PathPrefix: "/bucket", AllowLoopbackHTTP: true},
			wantOrigin: "http://127.0.0.1:9000",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, err := NewUploadTarget(c.spec)
			if err != nil {
				t.Fatalf("NewUploadTarget: %v", err)
			}
			if got := target.Origin(); got != c.wantOrigin {
				t.Fatalf("origin = %q, want %q", got, c.wantOrigin)
			}
		})
	}
}

func TestNewUploadTargetRejects(t *testing.T) {
	cases := map[string]TargetSpec{
		"plain http":                    {Origin: "http://archive.example.invalid", Addressing: VirtualHosted},
		"http on a non-loopback host":   {Origin: "http://archive.example.invalid", Addressing: VirtualHosted, AllowLoopbackHTTP: true},
		"wildcard host":                 {Origin: "https://*.example.invalid", Addressing: VirtualHosted},
		"user information":              {Origin: "https://user:pass@archive.example.invalid", Addressing: VirtualHosted},
		"fragment":                      {Origin: "https://archive.example.invalid#frag", Addressing: VirtualHosted},
		"query":                         {Origin: "https://archive.example.invalid?a=b", Addressing: VirtualHosted},
		"path":                          {Origin: "https://archive.example.invalid/bucket", Addressing: VirtualHosted},
		"opaque":                        {Origin: "https:archive.example.invalid", Addressing: VirtualHosted},
		"no host":                       {Origin: "https://", Addressing: VirtualHosted},
		"unknown scheme":                {Origin: "s3://archive.example.invalid", Addressing: VirtualHosted},
		"unknown addressing":            {Origin: "https://archive.example.invalid", Addressing: Addressing("dns-style")},
		"virtual-hosted with a prefix":  {Origin: "https://archive.example.invalid", Addressing: VirtualHosted, PathPrefix: "/bucket"},
		"path-style without a prefix":   {Origin: "https://minio.example.invalid", Addressing: PathStyle},
		"path-style root prefix":        {Origin: "https://minio.example.invalid", Addressing: PathStyle, PathPrefix: "/"},
		"path-style relative prefix":    {Origin: "https://minio.example.invalid", Addressing: PathStyle, PathPrefix: "bucket"},
		"path-style trailing slash":     {Origin: "https://minio.example.invalid", Addressing: PathStyle, PathPrefix: "/bucket/"},
		"path-style dot segment":        {Origin: "https://minio.example.invalid", Addressing: PathStyle, PathPrefix: "/bucket/../other"},
		"path-style escaped prefix":     {Origin: "https://minio.example.invalid", Addressing: PathStyle, PathPrefix: "/buck%65t"},
		"loopback flag on a real host":  {Origin: "https://localhost.example.invalid", Addressing: VirtualHosted, AllowLoopbackHTTP: true, PathPrefix: "/bucket"},
		"loopback name without a flag":  {Origin: "http://localhost:9000", Addressing: PathStyle, PathPrefix: "/bucket"},
		"loopback address without flag": {Origin: "http://[::1]:9000", Addressing: PathStyle, PathPrefix: "/bucket"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewUploadTarget(spec); err == nil {
				t.Fatalf("NewUploadTarget accepted %+v", spec)
			}
		})
	}
}

func TestUploadTargetListMatch(t *testing.T) {
	archive, err := NewUploadTarget(TargetSpec{Origin: "https://archive.example.invalid", Addressing: VirtualHosted})
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	minio, err := NewUploadTarget(TargetSpec{Origin: "https://minio.example.invalid:9000", Addressing: PathStyle, PathPrefix: "/trajectories"})
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	list := UploadTargetList{archive, minio}

	matched := []struct {
		url  string
		want string
	}{
		{"https://archive.example.invalid/v1/object.age?X-Amz-Signature=abc", archive.Origin()},
		{"https://archive.example.invalid:443/v1/object.age", archive.Origin()},
		{"https://ARCHIVE.example.invalid/v1/object.age", archive.Origin()},
		{"https://minio.example.invalid:9000/trajectories/v1/object.age", minio.Origin()},
	}
	for _, c := range matched {
		got, err := list.Match(c.url)
		if err != nil {
			t.Fatalf("Match(%q): %v", c.url, err)
		}
		if got.Origin() != c.want {
			t.Fatalf("Match(%q) = %s, want %s", c.url, got.Origin(), c.want)
		}
	}

	refused := map[string]string{
		"other host":       "https://evil.example.invalid/v1/object.age",
		"other port":       "https://archive.example.invalid:8443/v1/object.age",
		"other scheme":     "http://archive.example.invalid/v1/object.age",
		"user information": "https://user@archive.example.invalid/v1/object.age",
		"fragment":         "https://archive.example.invalid/v1/object.age#frag",
		"opaque":           "https:archive.example.invalid",
		"no host":          "/v1/object.age",
		"unparseable":      "https://archive.example.invalid/v1/%zz.age",
	}
	for name, raw := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := list.Match(raw); err == nil {
				t.Fatalf("Match accepted %q", raw)
			}
		})
	}

	if _, err := list.Match("https://evil.example.invalid/v1/object.age"); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("unlisted origin returned %v, want ErrNoTarget", err)
	}
}

// An empty allowlist is unpinned: only a configured entry may admit anything weaker than https.
func TestEmptyAllowlistAdoptsTheTicketOrigin(t *testing.T) {
	target, err := (UploadTargetList{}).Match("https://archive.example.invalid/v1/object.age")
	if err != nil {
		t.Fatalf("unpinned match: %v", err)
	}
	if target.addressing != addressingUnpinned {
		t.Error("the adopted target is not marked unpinned")
	}
	if got := target.Origin(); got != "https://archive.example.invalid:443" {
		t.Errorf("adopted origin %q", got)
	}

	for name, raw := range map[string]string{
		"http":          "http://archive.example.invalid/v1/object.age",
		"http loopback": "http://127.0.0.1:9000/bucket/v1/object.age",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (UploadTargetList{}).Match(raw); err == nil {
				t.Fatalf("unpinned mode accepted %q", raw)
			}
		})
	}
}

// The origin is diagnosable; the rest of a presigned URL is a bearer credential.
func TestMatchErrorCarriesNoURL(t *testing.T) {
	target, err := NewUploadTarget(TargetSpec{Origin: "https://archive.example.invalid", Addressing: VirtualHosted})
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	raw := "https://evil.example.invalid/v1/organization%3Dacme/object.age?X-Amz-Signature=deadbeef"
	_, err = UploadTargetList{target}.Match(raw)
	if err == nil {
		t.Fatal("Match accepted an unlisted origin")
	}
	assertNoURLLeak(t, err, raw)
}

func assertNoURLLeak(t *testing.T, err error, rawURL string) {
	t.Helper()
	message := err.Error()
	for _, secret := range []string{rawURL, "X-Amz-Signature", "deadbeef", "FIXTURE", "?"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %s", secret, message)
		}
	}
}
