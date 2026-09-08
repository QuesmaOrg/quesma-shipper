package app

import (
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// CatchUpDelay replaces the tick interval after a truncated run: max_files_per_run keeps one run
// short rather than rationing throughput, and a short pause keeps every per-run property intact.
const CatchUpDelay = 15 * time.Second

// NextDelay picks the sleep before the next flush. Only a clean-but-truncated run earns the
// catch-up delay: re-ticking fast on a failure or a panic turns one broken tick into a hot loop.
func NextDelay(rep formats.Report, err error, panicked bool, tick time.Duration) time.Duration {
	// PROGRESS is the condition, not merely truncation: per-file failures return no error, so
	// requiring something to have shipped keeps catch-up meaning "more of what just worked".
	if err == nil && !panicked && rep.Truncated && rep.Shipped > 0 {
		return CatchUpDelay
	}
	return tick
}
