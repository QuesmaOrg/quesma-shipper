package transforms

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
)

type recordSpool struct {
	bytes.Buffer
	writes []int
	wait   <-chan struct{}
	err    error
}

func (w *recordSpool) Write(p []byte) (int, error) {
	w.writes = append(w.writes, len(p))
	if w.wait != nil {
		<-w.wait
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.Buffer.Write(p)
}

func (*recordSpool) Seek(int64, int) (int64, error) { return 0, nil }

func TestExceptionalRecordThresholdIncludesLineEnding(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{127, 128, 129} {
		raw := strings.Repeat(" ", size-3) + "{}\n"
		_, _, err := s.scrubTo(context.Background(), &recordSpool{}, strings.NewReader(raw), Hint{JSONL: true}, 0, 128, make(chan struct{}, 1))
		if (err != nil) != (size > 128) {
			t.Fatalf("record size %d: expected only records above the threshold to require a cap, got %v", size, err)
		}
	}
}

func TestExceptionalRecordsMatchWholeFile(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	hint := Hint{Family: "claude-code", JSONL: true}
	prefix := "{\"text\":\"before alice@example.com\"}\r\n"
	large := `{"text":"` + strings.Repeat("ordinary words ", 20000) + `alice@example.com"}` + "\r\n"
	for _, tail := range []string{"{}\n", `{"tail":"alice@example.com`} {
		raw := prefix + large + large + tail
		want, err := s.Scrub([]byte(raw), hint)
		if err != nil {
			t.Fatal(err)
		}
		spool := &recordSpool{}
		got, hash, err := s.scrubTo(context.Background(), spool, strings.NewReader(raw), hint, int64(len(raw)), 128, make(chan struct{}, 1))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(spool.Bytes(), want.Out.Bytes()) || hash != Hash([]byte(raw)) || got.Out.hash != Hash(spool.Bytes()) {
			t.Fatal("exceptional records changed scrubbed bytes or hashes")
		}
		if len(spool.writes) != 4 {
			t.Fatalf("prefix, each exceptional record and tail must be scrubbed separately: %v", spool.writes)
		}
		got.Out, want.Out = Scrubbed{}, Scrubbed{}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ledger differs: got %+v, want %+v", got, want)
		}
	}
}

func TestExceptionalRecordsAreSerializedAndWaitIsCancelable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := New(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		slot := make(chan struct{}, 1)
		release := make(chan struct{})
		first := &recordSpool{wait: release}
		raw := `{"text":"` + strings.Repeat("ordinary words ", 20000) + `"}` + "\n"
		start := func(ctx context.Context, spool *recordSpool, input io.Reader) <-chan error {
			done := make(chan error, 1)
			go func() {
				_, _, err := s.scrubTo(ctx, spool, input, Hint{JSONL: true}, int64(len(raw)), 128, slot)
				done <- err
			}()
			return done
		}
		firstDone := start(context.Background(), first, strings.NewReader(raw))
		synctest.Wait()
		if len(first.writes) != 1 {
			t.Fatal("first exceptional record did not reach its scrubbed write")
		}
		ctx, cancel := context.WithCancel(context.Background())
		second := &recordSpool{}
		input := strings.NewReader(raw)
		secondDone := start(ctx, second, input)
		synctest.Wait()
		if len(second.writes) != 0 || len(raw)-input.Len() > 64<<10 {
			t.Fatal("second exceptional record was read or scrubbed while the first held the slot")
		}
		// Ordinary records do not wait for the exceptional record's write.
		if err := <-start(context.Background(), &recordSpool{}, strings.NewReader("{}\n")); err != nil {
			t.Fatal(err)
		}
		cancel()
		synctest.Wait()
		if err := <-secondDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting reader ignored cancellation: %v", err)
		}
		close(release)
		synctest.Wait()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
		if err := <-start(context.Background(), &recordSpool{}, strings.NewReader(raw)); err != nil {
			t.Fatalf("slot was not released after success: %v", err)
		}
	})
}

func TestExceptionalRecordErrorsReleaseSlotAndKeepFileCap(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"text":"` + strings.Repeat("ordinary words ", 20000) + `"}` + "\n"
	for _, kind := range []string{"write", "read", "scrub", "record cap", "file cap", "no cap"} {
		t.Run(kind, func(t *testing.T) {
			slot := make(chan struct{}, 1)
			spool := &recordSpool{}
			var input io.Reader = strings.NewReader(raw)
			cap := int64(len(raw))
			switch kind {
			case "write":
				spool.err = errors.New("disk full")
			case "read":
				input = io.MultiReader(strings.NewReader(raw[:len(raw)-2]), brokenReader{})
			case "scrub":
				input = strings.NewReader("PK\x03\x04" + raw[4:])
			case "record cap":
				cap--
			case "file cap":
				input = strings.NewReader(raw + raw)
			case "no cap":
				cap = 0
			}
			result, _, err := s.scrubTo(context.Background(), spool, input, Hint{JSONL: true}, cap, 128, slot)
			if err == nil || result.Out.via != "" {
				t.Fatalf("%s must fail closed: %v", kind, err)
			}
			if len(slot) != 0 {
				t.Fatalf("%s leaked the exceptional-record slot", kind)
			}
			if kind == "file cap" && !strings.Contains(err.Error(), "max_file_bytes") {
				t.Fatalf("whole-file cap was lost: %v", err)
			}
		})
	}
}
