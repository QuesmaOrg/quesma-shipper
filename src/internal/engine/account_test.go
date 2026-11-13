package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

func TestAccountHistoryUploadsFromMemoryAndRetriesCurrentUsage(t *testing.T) {
	f := newFixture(t)
	home := filepath.Join(f.home, ".codex")
	writeFile(t, filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey"}`))
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := catalog.Source("codex-account")
	o := f.opts()
	o.Plan.Interval = 5 * time.Minute
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
	writeFile(t, filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"fresh","tokens":{"id_token":"x.eyJlbWFpbCI6ImRldkBleGFtcGxlLm9yZyJ9.x"}}`))
	f.reopen()
	f.port.FailAll = nil
	rep := runEnrich(t, f, o)
	if len(rep.Sources) != 1 {
		t.Fatalf("expected one account source, got %d", len(rep.Sources))
	}
	if rep.Shipped != 1 || rep.Sources[0].Unreadable != 1 || rep.Sources[0].Reason == "" {
		t.Fatalf("retry current bucket must upload and report partial snapshot: %+v", rep)
	}
	keys := f.port.keys()
	if len(keys) != 1 {
		t.Fatalf("history keys %v", keys)
	}
	for _, key := range keys {
		manifest, payload := f.open(key)
		if manifest.Derived || manifest.Enricher != nil || manifest.Gather != "account" || manifest.PayloadMTime == nil {
			t.Fatalf("not a collector: %+v", manifest)
		}
		lines := strings.Split(payload, "\n")
		if len(lines) != 4 || len(lines[3]) != 0 {
			t.Fatalf("expected three newline-terminated records: %s", payload)
		}
		for _, line := range lines[:3] {
			if !json.Valid([]byte(line)) || !strings.Contains(line, `"bucket_start":`) {
				t.Fatalf("invalid account record: %s", line)
			}
		}
		if !strings.Contains(payload, `"auth_mode":"fresh"`) || !strings.Contains(payload, "dev@example.org") || manifest.Redaction != nil {
			t.Fatalf("fresh account payload altered: %s", payload)
		}
	}
	f.reopen()
	now = now.Add(time.Minute)
	rep = runEnrich(t, f, o)
	if rep.Shipped != 0 || rep.Unchanged != 1 || len(f.port.keys()) != 1 {
		t.Fatalf("same bucket should skip uploaded object: %+v", rep)
	}
	now = now.Add(5 * time.Minute)
	rep = runEnrich(t, f, o)
	if rep.Shipped != 0 || rep.Unchanged != 1 || len(f.port.keys()) != 1 {
		t.Fatalf("unchanged account in a new bucket should not upload: %+v", rep)
	}
	writeFile(t, filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"changed"}`))
	now = now.Add(5 * time.Minute)
	rep = runEnrich(t, f, o)
	if rep.Shipped != 1 {
		t.Fatalf("changed account in a new bucket %+v", rep)
	}
	if len(f.port.keys()) != 2 {
		t.Fatal("remote history overwritten")
	}
	f.wipeState()
	now = now.Add(5 * time.Minute)
	if rep = runEnrich(t, f, o); rep.Shipped != 1 || len(f.port.keys()) != 3 {
		t.Fatalf("a lost state file must ship the current snapshot again: %+v", rep)
	}
	now = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	if rep = runEnrich(t, f, o); rep.Shipped != 1 || len(f.port.keys()) != 4 {
		t.Fatalf("a new day must ship an unchanged account once: %+v", rep)
	}
	now = now.Add(5 * time.Minute)
	if rep = runEnrich(t, f, o); rep.Shipped != 0 || rep.Unchanged != 1 {
		t.Fatalf("the daily upload must not repeat within the day: %+v", rep)
	}
}

func TestAccountNewBucketRetriesDespiteBackoff(t *testing.T) {
	f := newFixture(t)
	// A Cursor root without its database fails every load, which parks the source.
	root := filepath.Join(f.home, "cursor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := catalog.Source("cursor-account")
	o := f.opts()
	o.Plan.Interval = 5 * time.Minute
	o.Env = sources.Env{Home: f.home, Lookup: func(string) (string, bool) { return "", false }}
	o.Plan.Sources = []sources.Resolved{{Source: spec, Root: root, Enabled: true, SpecFingerprint: sources.SpecFingerprint(spec)}}
	now := o.Now().Truncate(5 * time.Minute)
	o.Now = func() time.Time { return now }
	// Repeated failures in one bucket grow the backoff past the bucket's end.
	for range 4 {
		runEnrich(t, f, o)
		f.reopen()
		now = now.Add(time.Minute)
	}
	writeFile(t, filepath.Join(root, "state.vscdb"), nil)
	now = now.Truncate(5 * time.Minute).Add(5 * time.Minute)
	if rep := runEnrich(t, f, o); rep.Shipped != 1 {
		t.Fatalf("a new bucket must not wait out the previous bucket's backoff: %+v", rep)
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
	writeFile(t, filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey"}`))
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
				m, payload := f.open(key)
				if tc.wantRaw {
					if payload != raw || m.Redaction != nil {
						t.Fatal("disabled scrub changed bytes or reported redaction")
					}
				} else if strings.Contains(payload, "dev@example.org") || strings.Contains(payload, "fixture-secret") || m.Redaction == nil {
					t.Fatalf("scrubbing was not applied: %s", payload)
				}
			}
		})
	}
}
