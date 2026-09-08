package upload

import (
	"errors"
	"maps"
	"strings"
	"testing"
)

func TestValidateTicketAcceptsGolden(t *testing.T) {
	for _, pair := range []struct{ request, response string }{
		{"request.json", "response.json"},
		{"request-heartbeat.json", "response-heartbeat.json"},
	} {
		t.Run(pair.request, func(t *testing.T) {
			prepared, ticket := goldenPair(t, pair.request, pair.response)
			if err := ValidateTicket(goldenTarget(t), prepared, ticket); err != nil {
				t.Fatalf("golden ticket refused: %v", err)
			}
		})
	}
}

func TestValidateTicketAcceptsEveryProviderDialect(t *testing.T) {
	prepared, base := goldenPair(t, "request.json", "response.json")
	for _, tc := range []struct {
		name      string
		mapHeader func(string) (string, bool)
		extra     map[string]string
	}{
		{name: "aws", mapHeader: func(name string) (string, bool) { return name, true }},
		{name: "gcs", mapHeader: func(name string) (string, bool) {
			if name == taggingHeader {
				return "", false
			}
			return strings.Replace(name, metadataPrefix, "x-goog-meta-", 1), true
		}},
		{name: "azure", mapHeader: func(name string) (string, bool) {
			if name == taggingHeader {
				return "x-ms-tags", true
			}
			name = strings.TrimPrefix(name, metadataPrefix)
			return "x-ms-meta-" + strings.ReplaceAll(name, "-", "_"), true
		}, extra: map[string]string{"x-ms-blob-type": "BlockBlob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ticket := base
			ticket.RequiredHeaders = make(map[string]string, len(base.RequiredHeaders)+len(tc.extra))
			for name, value := range base.RequiredHeaders {
				if mapped, ok := tc.mapHeader(name); ok {
					ticket.RequiredHeaders[mapped] = value
				}
			}
			for name, value := range tc.extra {
				ticket.RequiredHeaders[name] = value
			}
			if err := ValidateTicket(goldenTarget(t), prepared, ticket); err != nil {
				t.Fatalf("%s ticket refused: %v", tc.name, err)
			}
		})
	}
}

func TestValidateTicketRejects(t *testing.T) {
	base, baseTicket := goldenPair(t, "request.json", "response.json")
	target := goldenTarget(t)
	pathOf := func(key string) string {
		return "https://archive.example.invalid/" + canonicalPath(key) + "?X-Amz-Signature=FIXTURE"
	}

	cases := []struct {
		name    string
		mutate  func(prepared *PreparedUpload, ticket *Ticket)
		wantErr error
	}{
		{"object id mismatch", func(_ *PreparedUpload, tk *Ticket) { tk.ObjectID = "trajectory-2" }, nil},
		{"method is not PUT", func(_ *PreparedUpload, tk *Ticket) { tk.Method = "POST" }, nil},
		{"content length mismatch", func(_ *PreparedUpload, tk *Ticket) { tk.ContentLength++ }, nil},
		{"unlisted origin", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "archive.example.invalid", "evil.example.invalid", 1)
		}, ErrNoTarget},
		{"unlisted port", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "archive.example.invalid", "archive.example.invalid:8443", 1)
		}, ErrNoTarget},
		{"sibling install key", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
				"11111111-2222-3333-4444-555555555555", 1)
		}, nil},
		{"key suffix confusion", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, ".age?", ".age.evil?", 1)
		}, nil},
		{"unescaped separator", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.ReplaceAll(tk.URL, "%3D", "=")
		}, nil},
		{"lowercase escape", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.ReplaceAll(tk.URL, "%3D", "%3d")
		}, nil},
		{"duplicate slash", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "/mirror/", "//mirror/", 1)
		}, nil},
		{"dot segment", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "/mirror/", "/mirror/./", 1)
		}, nil},
		{"parent segment", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "/mirror/", "/mirror/../", 1)
		}, nil},
		{"encoded slash", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "/mirror/", "%2Fmirror/", 1)
		}, nil},
		{"encoded backslash", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "/mirror/", "/mirror%5C", 1)
		}, nil},
		{"user information", func(_ *PreparedUpload, tk *Ticket) {
			tk.URL = strings.Replace(tk.URL, "https://", "https://user:pass@", 1)
		}, nil},
		{"fragment", func(_ *PreparedUpload, tk *Ticket) { tk.URL += "#frag" }, nil},
		{"opaque url", func(_ *PreparedUpload, tk *Ticket) { tk.URL = "https:archive.example.invalid" }, nil},
		{"prepared key not canonical", func(p *PreparedUpload, tk *Ticket) {
			p.Key = "/" + p.Key
			tk.URL = pathOf(p.Key)
		}, nil},
		{"unknown provider header", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders["x-amz-acl"] = "public-read"
		}, nil},
		{"unknown metadata header", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders["x-amz-meta-operator"] = "someone"
		}, nil},
		{"missing source hash header", func(_ *PreparedUpload, tk *Ticket) {
			delete(tk.RequiredHeaders, sourceHashHeader)
		}, nil},
		{"missing ticket id header", func(_ *PreparedUpload, tk *Ticket) {
			delete(tk.RequiredHeaders, ticketIDHeader)
		}, nil},
		{"source hash disagrees with the prepared object", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders[sourceHashHeader] = strings.Repeat("a", 64)
		}, nil},
		{"ticket id header disagrees with the ticket", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders[ticketIDHeader] = "00000000-0000-0000-0000-000000000000"
		}, nil},
		{"metadata value disagrees", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders["x-amz-meta-source-id"] = "another-source"
		}, nil},
		{"metadata the prepared object never declared", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders["x-amz-meta-agent-version"] = "9.9.9"
		}, nil},
		{"metadata dropped from the ticket", func(_ *PreparedUpload, tk *Ticket) {
			delete(tk.RequiredHeaders, "x-amz-meta-source-id")
		}, nil},
		{"tag outside the closed set", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders[taggingHeader] = "class=anything"
		}, nil},
		{"empty tag value", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders[taggingHeader] = ""
		}, nil},
		{"emptied metadata value", func(_ *PreparedUpload, tk *Ticket) {
			tk.RequiredHeaders["x-amz-meta-artifact-class"] = ""
		}, nil},
		{"prepared metadata outside the closed set", func(p *PreparedUpload, tk *Ticket) {
			p.Metadata["operator"] = "someone"
			tk.RequiredHeaders["x-amz-meta-operator"] = "someone"
		}, nil},
		{"prepared object carries no source hash", func(p *PreparedUpload, _ *Ticket) { p.SourceHash = "" }, nil},
		{"ticket carries no ticket id", func(_ *PreparedUpload, tk *Ticket) { tk.TicketID = "" }, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prepared, ticket := base, baseTicket
			prepared.Metadata = maps.Clone(base.Metadata)
			ticket.RequiredHeaders = maps.Clone(baseTicket.RequiredHeaders)
			c.mutate(&prepared, &ticket)

			err := ValidateTicket(target, prepared, ticket)
			if err == nil {
				t.Fatalf("ValidateTicket accepted the %s case", c.name)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("error is %v, want %v", err, c.wantErr)
			}
			assertNoURLLeak(t, err, ticket.URL)
		})
	}
}

func TestValidateTicketPathStyle(t *testing.T) {
	target, err := NewUploadTarget(TargetSpec{
		Origin:     "https://minio.example.invalid:9000",
		Addressing: PathStyle,
		PathPrefix: "/trajectories",
	})
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	prepared, ticket := goldenPair(t, "request.json", "response.json")
	origin := "https://minio.example.invalid:9000"
	ticket.URL = origin + "/trajectories/" + canonicalPath(prepared.Key) + "?X-Amz-Signature=FIXTURE"
	if err := ValidateTicket(target, prepared, ticket); err != nil {
		t.Fatalf("path-style ticket refused: %v", err)
	}

	refused := map[string]string{
		"bucket prefix missing":   origin + "/" + canonicalPath(prepared.Key),
		"neighbouring bucket":     origin + "/trajectories-staging/" + canonicalPath(prepared.Key),
		"prefix without boundary": origin + "/trajectoriesx/" + canonicalPath(prepared.Key),
		"prefix repeated":         origin + "/trajectories/trajectories/" + canonicalPath(prepared.Key),
	}
	for name, raw := range refused {
		t.Run(name, func(t *testing.T) {
			ticket.URL = raw
			if err := ValidateTicket(target, prepared, ticket); err == nil {
				t.Fatalf("path-style validation accepted %q", raw)
			}
		})
	}
}

// With no configured targets, the exact-key check accepts the one spelling either form would
// produce: the key at the root, or below exactly one bucket segment, and nothing else.
func TestValidateTicketUnpinned(t *testing.T) {
	prepared, ticket := goldenPair(t, "request.json", "response.json")
	unpinned := func(t *testing.T, rawURL string) UploadTarget {
		t.Helper()
		target, err := (UploadTargetList{}).Match(rawURL)
		if err != nil {
			t.Fatalf("unpinned match: %v", err)
		}
		return target
	}

	// The golden ticket as issued: virtual-hosted spelling.
	if err := ValidateTicket(unpinned(t, ticket.URL), prepared, ticket); err != nil {
		t.Fatalf("unpinned virtual-hosted ticket refused: %v", err)
	}

	// The same key below exactly one bucket segment: path-style spelling.
	origin := "https://minio.example.invalid:9000"
	ticket.URL = origin + "/trajectories/" + canonicalPath(prepared.Key) + "?X-Amz-Signature=FIXTURE"
	if err := ValidateTicket(unpinned(t, ticket.URL), prepared, ticket); err != nil {
		t.Fatalf("unpinned path-style ticket refused: %v", err)
	}

	refused := map[string]string{
		"two bucket segments":  origin + "/a/b/" + canonicalPath(prepared.Key),
		"empty bucket segment": origin + "//" + canonicalPath(prepared.Key),
		"sibling install key": origin + "/trajectories/" + canonicalPath(strings.Replace(prepared.Key,
			"3f2504e0-4f89-41d3-9a0c-0305e82c3301", "11111111-2222-3333-4444-555555555555", 1)),
		"key suffix confusion": origin + "/" + canonicalPath(prepared.Key) + ".evil",
	}
	for name, raw := range refused {
		t.Run(name, func(t *testing.T) {
			ticket.URL = raw
			if err := ValidateTicket(unpinned(t, raw), prepared, ticket); err == nil {
				t.Fatalf("unpinned validation accepted %q", raw)
			}
		})
	}
}

// An addressing form nobody taught the validator is a refusal, never a pass-through.
func TestValidateTicketUnknownAddressing(t *testing.T) {
	prepared, ticket := goldenPair(t, "request.json", "response.json")
	target := goldenTarget(t)
	target.addressing = Addressing("dns-style")
	if err := ValidateTicket(target, prepared, ticket); err == nil {
		t.Fatal("unknown addressing was accepted")
	}
}
