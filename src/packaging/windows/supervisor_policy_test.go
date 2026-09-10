package windows

import (
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func TestSupervisorRestartPolicy(t *testing.T) {
	if delay, crashes, restart := restartPolicy(common.SupervisorRestartExitCode, 7); !restart || delay != 0 || crashes != 0 {
		t.Fatalf("intentional restart = (%s, %d, %v)", delay, crashes, restart)
	}

	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	crashes := 0
	for i, wantDelay := range want {
		delay, next, restart := restartPolicy(1, crashes)
		if !restart || delay != wantDelay || next != i+1 {
			t.Fatalf("crash %d = (%s, %d, %v), want (%s, %d, true)",
				i+1, delay, next, restart, wantDelay, i+1)
		}
		crashes = next
	}
	if delay, next, restart := restartPolicy(1, crashes); restart || delay != 0 || next != maxCrashCount {
		t.Fatalf("give-up = (%s, %d, %v)", delay, next, restart)
	}
}
