package windows

import (
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func TestSupervisorRestartPolicy(t *testing.T) {
	if delay, crashes, restart := restartPolicy(common.SupervisorRestartExitCode, 0, 7); !restart || delay != 0 || crashes != 0 {
		t.Fatalf("intentional restart = (%s, %d, %v)", delay, crashes, restart)
	}

	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	crashes := 0
	for i, wantDelay := range want {
		delay, next, restart := restartPolicy(1, 0, crashes)
		if !restart || delay != wantDelay || next != i+1 {
			t.Fatalf("crash %d = (%s, %d, %v), want (%s, %d, true)",
				i+1, delay, next, restart, wantDelay, i+1)
		}
		crashes = next
	}
	if delay, next, restart := restartPolicy(1, 0, crashes); restart || delay != 0 || next != maxCrashCount {
		t.Fatalf("give-up = (%s, %d, %v)", delay, next, restart)
	}
}

func TestAHealthyRunResetsTheCrashBudget(t *testing.T) {
	if _, next, restart := restartPolicy(1, healthyUptime, maxCrashCount-1); !restart || next != 1 {
		t.Fatalf("after a healthy run = (%d, %v), want (1, true)", next, restart)
	}
	if _, _, restart := restartPolicy(1, healthyUptime-time.Second, maxCrashCount-1); restart {
		t.Fatal("a short run must still exhaust the budget")
	}
}
