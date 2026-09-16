package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestAccountHistoryUploadsFromMemoryAndRetriesCurrentUsage(t *testing.T) {
	f := newFixture(t)
	home := filepath.Join(f.home, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey"}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := catalog.Source("codex-account")
	o := f.opts()
	o.Env = sources.Env{Home: f.home, Lookup: func(string) (string, bool) { return "", false }}
	o.Plan.Sources = []sources.Resolved{{Source: spec, Root: home, Enabled: true, SpecFingerprint: sources.SpecFingerprint(spec)}}
	now := o.Now()
	o.Now = func() time.Time { return now }
	f.port.FailAll = errors.New("offline")
	if _, err := engine.Run(context.Background(), f.store, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, "snapshots")); !os.IsNotExist(err) {
		t.Fatal("account collection staged files")
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"fresh","tokens":{"id_token":"x.eyJlbWFpbCI6ImRldkBleGFtcGxlLm9yZyJ9.x"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	f.port.FailAll = nil
	rep := runEnrich(t, f, o)
	if rep.Shipped != 1 {
		t.Fatalf("retry current bucket: %+v", rep)
	}
	keys := f.port.keys()
	if len(keys) != 1 {
		t.Fatalf("history keys %v", keys)
	}
	for _, key := range keys {
		obj, _ := f.port.get(key)
		manifest, payload, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Derived || manifest.Enricher != nil || manifest.Gather != "account" || manifest.PayloadMTime == nil {
			t.Fatalf("not a collector: %+v", manifest)
		}
		lines := bytes.Split(payload, []byte("\n"))
		if len(lines) != 3 || len(lines[2]) != 0 {
			t.Fatalf("expected two newline-terminated records: %s", payload)
		}
		for _, line := range lines[:2] {
			if !json.Valid(line) || !bytes.Contains(line, []byte(`"bucket_start":`)) {
				t.Fatalf("invalid account record: %s", line)
			}
		}
		if !bytes.Contains(payload, []byte(`"auth_mode":"fresh"`)) || !bytes.Contains(payload, []byte("dev@example.org")) || manifest.Redaction != nil {
			t.Fatalf("fresh account payload altered: %s", payload)
		}
	}
	f.reopen()
	now = now.Add(time.Minute)
	rep = runEnrich(t, f, o)
	if rep.Shipped != 1 || len(f.port.keys()) != 1 {
		t.Fatalf("same bucket should overwrite existing object: %+v", rep)
	}
	now = now.Add(15 * time.Minute)
	rep = runEnrich(t, f, o)
	if rep.Shipped != 1 {
		t.Fatalf("new bucket %+v", rep)
	}
	if len(f.port.keys()) != 2 {
		t.Fatal("remote history overwritten")
	}
}

func TestAccountDisabledAndPreviewDoNotCapture(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.Env = sources.Env{Home: f.home, Lookup: func(string) (string, bool) { return "", false }}
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := catalog.Source("codex-account")
	home := filepath.Join(f.home, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey"}`), 0600); err != nil {
		t.Fatal(err)
	}
	o.Plan.Sources = []sources.Resolved{{Source: spec, Root: home, Enabled: false}}
	runEnrich(t, f, o)
	o.Plan.Sources[0].Enabled = true
	o.DryRun = true
	runEnrich(t, f, o)
	if _, err := os.Stat(filepath.Join(f.stateDir, "snapshots")); !os.IsNotExist(err) {
		t.Fatal("disabled/preview collection wrote snapshots")
	}
}

func TestSourceScrubSetting(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name    string
		setting *bool
		wantRaw bool
	}{{"default", nil, false}, {"enabled", &on, false}, {"disabled", &off, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			const raw = "{\"email\":\"dev@example.org\",\"access_token\":\"fixture-secret\"}\n"
			f.writeTranscript("projects/demo/session.jsonl", raw)
			o := f.opts()
			o.Plan.Sources[0].Scrub = tc.setting
			rep := runEnrich(t, f, o)
			if rep.Shipped != 1 {
				t.Fatalf("shipped: %+v", rep)
			}
			for _, key := range f.port.keys() {
				obj, _ := f.port.get(key)
				m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
				if err != nil {
					t.Fatal(err)
				}
				if tc.wantRaw {
					if string(payload) != raw || m.Redaction != nil {
						t.Fatal("disabled scrub changed bytes or reported redaction")
					}
				} else if bytes.Contains(payload, []byte("dev@example.org")) || bytes.Contains(payload, []byte("fixture-secret")) || m.Redaction == nil {
					t.Fatalf("scrubbing was not applied: %s", payload)
				}
			}
		})
	}
}
