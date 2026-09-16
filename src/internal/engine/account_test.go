package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestAccountHistoryIsIndependentAndSurvivesFailedUpload(t *testing.T) {
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
	o.Plan.Sources = []sources.Resolved{{Source: spec, Enabled: true, SpecFingerprint: sources.SpecFingerprint(spec)}}
	now := o.Now()
	o.Now = func() time.Time { return now }
	f.port.FailAll = errors.New("offline")
	if _, err := engine.Run(context.Background(), f.store, o); err != nil {
		t.Fatal(err)
	}
	pending, err := filepath.Glob(filepath.Join(f.stateDir, "snapshots", "codex-account", "*.json"))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending %v %v", pending, err)
	}
	first, err := os.ReadFile(pending[0])
	if err != nil {
		t.Fatal(err)
	}
	f.reopen()
	f.port.FailAll = nil
	now = now.Add(15 * time.Minute)
	rep := runEnrich(t, f, o)
	if rep.Shipped != 2 {
		t.Fatalf("retry and new bucket: %+v", rep)
	}
	keys := f.port.keys()
	if len(keys) != 2 {
		t.Fatalf("history keys %v", keys)
	}
	found := false
	for _, key := range keys {
		obj, _ := f.port.get(key)
		manifest, payload, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Derived || manifest.Enricher != nil || manifest.Gather != "account" || manifest.PayloadMTime == nil {
			t.Fatalf("not a collector: %+v", manifest)
		}
		if string(payload) == string(first) {
			found = true
		}
	}
	if !found {
		t.Fatal("failed snapshot regenerated on retry")
	}
	rep = runEnrich(t, f, o)
	if rep.Shipped != 0 {
		t.Fatalf("same bucket reshipped %+v", rep)
	}
	now = now.Add(15 * time.Minute)
	rep = runEnrich(t, f, o)
	if rep.Shipped != 1 {
		t.Fatalf("new bucket %+v", rep)
	}
	if len(f.port.keys()) != 3 {
		t.Fatal("remote history overwritten")
	}
	if _, err := os.Stat(pending[0]); !os.IsNotExist(err) {
		t.Fatal("confirmed old snapshot not cleaned")
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
	o.Plan.Sources = []sources.Resolved{{Source: spec, Enabled: false}}
	runEnrich(t, f, o)
	o.Plan.Sources[0].Enabled = true
	o.DryRun = true
	runEnrich(t, f, o)
	if _, err := os.Stat(filepath.Join(f.stateDir, "snapshots")); !os.IsNotExist(err) {
		t.Fatal("disabled/preview collection wrote snapshots")
	}
}
