//go:build chaos

// The results matrix: one row per scenario, printed when the suite ends and written as text and JSON
// (where: CONTRIBUTING.md), so a run leaves its evidence; a scenario that never recorded is listed too.
package chaos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
)

const resultsEnv = "SHIPPER_CHAOS_RESULTS"

type row struct {
	order          int
	Scenario       string          `json:"scenario"`
	Field          string          `json:"field_fault"`
	Kind           string          `json:"kind"`
	Counted        bool            `json:"counted"`
	Consecutive    int             `json:"consecutive_failures"`
	TickSeconds    float64         `json:"tick_seconds"`
	Exit           int             `json:"exit"`
	Message        string          `json:"message"`
	Notes          string          `json:"notes,omitempty"`
	NotRecorded    bool            `json:"not_recorded,omitempty"`
	FaultTelemetry bool            `json:"fault_tick_telemetry_delivered"`
	FaultHealth    json.RawMessage `json:"fault_tick_install_health,omitempty"`
	RecoveryHealth json.RawMessage `json:"recovery_install_health,omitempty"`
}

var (
	rowsMu sync.Mutex
	rows   []row
)

func recordRow(r row) {
	rowsMu.Lock()
	defer rowsMu.Unlock()
	rows = append(rows, r)
}

func writeMatrix(t *testing.T) {
	rowsMu.Lock()
	defer rowsMu.Unlock()
	for i, sc := range scenarios {
		if !slices.ContainsFunc(rows, func(r row) bool { return r.order == i }) {
			rows = append(rows, row{order: i, Scenario: sc.title, Field: sc.field, NotRecorded: true,
				Notes: "not run, or failed before its fault tick was recorded"})
		}
	}
	slices.SortFunc(rows, func(a, b row) int { return a.order - b.order })

	var text strings.Builder
	tw := tabwriter.NewWriter(&text, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tFIELD\tKIND\tCOUNTED\tCONSECUTIVE\tTICK_S\tEXIT\tMESSAGE")
	for _, r := range rows {
		if r.NotRecorded {
			fmt.Fprintf(tw, "%s\t%s\tNOT RECORDED\t-\t-\t-\t-\t\n", r.Scenario, r.Field)
			continue
		}
		kind := r.Kind
		if kind == "" {
			kind = "(none)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%d\t%.1f\t%d\t%s\n",
			r.Scenario, r.Field, kind, r.Counted, r.Consecutive, r.TickSeconds, r.Exit, excerpt(r.Message))
	}
	_ = tw.Flush()
	for _, r := range rows {
		if r.Notes != "" {
			fmt.Fprintf(&text, "note: %s: %s\n", r.Scenario, r.Notes)
		}
	}
	fmt.Print("\n" + text.String())

	dir := filepath.Join(os.TempDir(), "trajectory-shipper-chaos")
	if v := os.Getenv(resultsEnv); v != "" {
		dir = v
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	full, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"matrix.txt": []byte(text.String()), "matrix.json": full} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	fmt.Printf("chaos matrix written to %s\n", dir)
}

// The cause sits at the end of a wrapped error, so the excerpt keeps the tail.
func excerpt(message string) string {
	const keep = 110
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) <= keep {
		return message
	}
	return "..." + message[len(message)-keep:]
}
