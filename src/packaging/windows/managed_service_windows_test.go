package windows

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	"golang.org/x/sys/windows"
)

func TestManagedTaskQueryRetriesOnlyOneTimeoutAndPreservesTheResult(t *testing.T) {
	unreadable := errors.New("access denied")
	timeout := fmt.Errorf("cannot verify absence: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name    string
		results []error
		want    error
	}{
		{"absent", []error{nil}, nil},
		{"managed", []error{common.ErrSystemManaged}, common.ErrSystemManaged},
		{"unreadable", []error{unreadable}, unreadable},
		{"canceled", []error{context.Canceled}, context.Canceled},
		{"recovered absent", []error{timeout, nil}, nil},
		{"recovered managed", []error{timeout, common.ErrSystemManaged}, common.ErrSystemManaged},
		{"recovered unreadable", []error{timeout, unreadable}, unreadable},
		{"still unavailable", []error{timeout, timeout}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryManagedTaskQuery(func() error {
				if calls == len(tc.results) {
					t.Fatal("unexpected extra query")
				}
				result := tc.results[calls]
				calls++
				return result
			})
			if calls != len(tc.results) || !errors.Is(err, tc.want) {
				t.Fatalf("queries=%d, error=%v; want queries=%d, error=%v", calls, err, len(tc.results), tc.want)
			}
		})
	}
}

func TestManagedTaskConflictQueryUsesTaskSchedulerWithoutModifyingIt(t *testing.T) {
	if err := checkNoManagedTask(); err != nil && !errors.Is(err, common.ErrSystemManaged) {
		t.Fatalf("read-only managed task query did not distinguish an existing task from absence: %v", err)
	}
}

func TestManagedTaskQueryPreservesExpiredDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if err := checkNoManagedTaskOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the real query must preserve the deadline error for its retry policy, got %v", err)
	}
}

func TestManagedSupervisorLockPreventsDuplicateCollectorsAndReleasesOnExit(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireSupervisorLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := acquireSupervisorLock(dir)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("duplicate supervisor lock = %v, want ERROR_LOCK_VIOLATION", err)
	}
	otherUser, err := acquireSupervisorLock(t.TempDir())
	if err != nil {
		t.Fatalf("another user's lock was blocked: %v", err)
	}
	otherUser.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := acquireSupervisorLock(dir)
	if err != nil {
		t.Fatalf("supervisor cannot restart after exit: %v", err)
	}
	restarted.Close()
}

func TestManagedTaskRecognizesTheLocalizedUsersGroup(t *testing.T) {
	sid, err := windows.StringToSid("S-1-5-32-545")
	if err != nil {
		t.Fatal(err)
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{sid.String(), domain + `\` + account} {
		if !isUsersGroup(value) {
			t.Fatalf("Users group %q was not recognized", value)
		}
	}
	for _, value := range []string{"S-1-5-18", "S-1-5-32-544", "S-1-1-0", ""} {
		if isUsersGroup(value) {
			t.Fatalf("unexpected managed principal %q was accepted", value)
		}
	}
}
