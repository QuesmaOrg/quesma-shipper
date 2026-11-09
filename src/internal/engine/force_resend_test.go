package engine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
)

func TestForceResendIncludesUnchangedRawAndDerivedObjects(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)
	o := enrichOpts(t, f, db, true)
	if rep := runEnrich(t, f, o); rep.Shipped != 2 {
		t.Fatalf("initial pair: %d", rep.Shipped)
	}
	now := time.Now().Add(48 * time.Hour)
	o.Now = func() time.Time { return now }
	o.ForceResend = true
	f.port.verdict = func(_, _ int, object engine.PreparedObject) error {
		if !object.ForceResend {
			t.Error("forced object lost its replacement request")
		}
		return nil
	}
	if rep := runEnrich(t, f, o); rep.Shipped != 2 {
		t.Fatalf("forced raw/derived pair: %d", rep.Shipped)
	}
	for _, key := range f.port.keys() {
		object, _ := f.port.get(key)
		if object.Versions != 2 {
			t.Fatalf("%s has %d versions", key, object.Versions)
		}
	}
}

func TestForceResendNeverCommitsAnAlreadyPresentAnswer(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			f := newFixture(t)
			f.writeTranscript("p/force.jsonl", line1)
			port := newPort()
			port.verdict = func(call, _ int, object engine.PreparedObject) error {
				if !object.ForceResend {
					t.Error("reauthorization lost force resend")
				}
				if expired && call == 0 {
					return engine.ErrTicketExpired
				}
				return engine.ErrAlreadyPresent
			}
			o := f.opts()
			o.Upload, o.ForceResend = port, true
			rep, err := engine.Run(context.Background(), f.store, o)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Shipped != 0 || rep.Failed != 1 || f.store.Len() != 0 {
				t.Fatalf("already-present answer committed a forced upload: %+v, fingerprints=%d", rep, f.store.Len())
			}
		})
	}
}
