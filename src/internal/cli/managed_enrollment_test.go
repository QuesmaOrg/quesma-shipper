package cli

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

func TestEnrollmentConflictsBackOffWithoutStoppingRecovery(t *testing.T) {
	var conflicts int
	for _, cap := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		delay := managedEnrollmentDelay(fmt.Errorf("recover: %w", controlplane.ErrEnrollmentConflict), &conflicts)
		if delay < cap/2 || delay >= cap {
			t.Fatalf("conflict retry delay %s outside [%s, %s)", delay, cap/2, cap)
		}
	}
	for _, err := range []error{nil, errors.New("server unavailable")} {
		if delay := managedEnrollmentDelay(err, &conflicts); delay != time.Minute || conflicts != 0 {
			t.Fatalf("ordinary polling did not resume: delay=%s conflicts=%d", delay, conflicts)
		}
	}
	if delay := managedEnrollmentDelay(controlplane.ErrEnrollmentConflict, &conflicts); delay < time.Minute || delay >= 2*time.Minute {
		t.Fatalf("new conflict did not restart backoff: %s", delay)
	}
}
