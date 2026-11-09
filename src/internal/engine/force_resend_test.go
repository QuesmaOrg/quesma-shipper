package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
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
	f.reopen()
	if rep := runEnrich(t, f, o); rep.Shipped != 0 {
		t.Fatalf("completed raw/derived pair repeated: %d", rep.Shipped)
	}
	for _, key := range f.port.keys() {
		object, _ := f.port.get(key)
		if object.Versions != 2 {
			t.Fatalf("%s has %d versions", key, object.Versions)
		}
	}
}

func TestForceResendParkedFileDoesNotRepeatSuccessfulFiles(t *testing.T) {
	f := newFixture(t)
	bad := f.writeTranscript("p/bad.jsonl", line1)
	f.writeTranscript("p/good.jsonl", line1)
	f.run()
	if err := os.WriteFile(bad, []byte("SQLite format 3\x00opaque"), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		f.reopen()
		rep := f.runWith(func(o *engine.Options) { o.ForceResend = true })
		if rep.Parked != 1 || rep.ResendOutstanding != 1 || rep.Shipped != 1-attempt {
			t.Fatalf("attempt %d: %+v", attempt, rep)
		}
	}
	f.writeTranscript("p/bad.jsonl", line1)
	f.reopen()
	rep := f.runWith(func(o *engine.Options) { o.ForceResend = true })
	if rep.Shipped != 1 || rep.Parked != 0 || rep.ResendOutstanding != 0 {
		t.Fatalf("repaired file: %+v", rep)
	}
	for _, key := range f.port.keys() {
		object, _ := f.port.get(key)
		if object.Versions != 2 {
			t.Fatalf("%s has %d versions", key, object.Versions)
		}
	}
}

func TestForceResendResumesAfterDeadline(t *testing.T) {
	f := newFixture(t)
	for i := range 4 {
		f.writeTranscript(fmt.Sprintf("p/%d.jsonl", i), line1)
	}
	f.run()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var completed atomic.Int32
	f.port.verdict = func(_, _ int, _ engine.PreparedObject) error {
		if completed.Add(1) == 1 {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	o := f.opts()
	o.ForceResend, o.Unbounded = true, true
	o.Workers, o.UploadWorkers, o.CommitBatch = 1, 1, 1
	rep, err := engine.Run(ctx, f.store, o)
	if !errors.Is(err, context.DeadlineExceeded) || rep.Shipped != 1 || rep.ResendOutstanding != 3 {
		t.Fatalf("deadline did not preserve partial progress: %+v, %v", rep, err)
	}
	f.port.verdict = nil
	f.reopen()
	rep = f.runWith(func(o *engine.Options) { o.ForceResend = true })
	if rep.Shipped != 3 || rep.ResendOutstanding != 0 {
		t.Fatalf("retry did not finish the remaining files: %+v", rep)
	}
	for _, key := range f.port.keys() {
		object, _ := f.port.get(key)
		if object.Versions != 2 {
			t.Fatalf("%s has %d versions", key, object.Versions)
		}
	}
}

func TestForceResendReportsExcludedAndOversizeHistory(t *testing.T) {
	for _, reason := range []string{"excluded", "oversize"} {
		t.Run(reason, func(t *testing.T) {
			f := newFixture(t)
			f.writeTranscript("p/session.jsonl", line1)
			f.run()
			rep := f.runWith(func(o *engine.Options) {
				o.ForceResend = true
				if reason == "excluded" {
					o.Plan.Sources[0].Enabled = false
				} else {
					o.Plan.Sources[0].MaxFileBytes = 1
				}
			})
			if rep.Shipped != 0 || rep.ResendOutstanding != 1 {
				t.Fatalf("unavailable history not reported: %+v", rep)
			}
		})
	}
}

func TestForceResendChangedContentStillShipsUnderSamePolicy(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/session.jsonl", line1)
	f.runWith(func(o *engine.Options) { o.ForceResend = true })
	f.writeTranscript("p/session.jsonl", line1+line1)
	rep := f.runWith(func(o *engine.Options) { o.ForceResend = true })
	if rep.Shipped != 1 {
		t.Fatalf("policy hid changed content: %+v", rep)
	}
	for _, source := range rep.Sources {
		for _, file := range source.Files {
			if file.Decision == formats.DecisionUnchanged {
				t.Fatal("changed content treated as unchanged")
			}
		}
	}
}

func TestForceResendNewPolicyInvalidatesOnlyForcedProgress(t *testing.T) {
	f := newFixture(t)
	f.writeTranscript("p/session.jsonl", line1)
	o := f.opts()
	o.ForceResend = true
	if rep, err := engine.Run(context.Background(), f.store, o); err != nil || rep.Shipped != 1 {
		t.Fatalf("first forced upload: %+v, %v", rep, err)
	}
	bob, _ := age.GenerateX25519Identity()
	for _, change := range []func(){
		func() { o.Recipients = []age.Recipient{bob.Recipient()} },
		func() { o.Plan.SecretKeyNames = []string{"HOUSE_SIG"} },
	} {
		change()
		f.reopen()
		o.ForceResend = false
		if rep, err := engine.Run(context.Background(), f.store, o); err != nil || rep.Shipped != 0 {
			t.Fatalf("normal run initiated a policy resend: %+v, %v", rep, err)
		}
		o.ForceResend = true
		if rep, err := engine.Run(context.Background(), f.store, o); err != nil || rep.Shipped != 1 {
			t.Fatalf("changed policy reused old progress: %+v, %v", rep, err)
		}
		if rep, err := engine.Run(context.Background(), f.store, o); err != nil || rep.Shipped != 0 {
			t.Fatalf("completed policy resent again: %+v, %v", rep, err)
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
