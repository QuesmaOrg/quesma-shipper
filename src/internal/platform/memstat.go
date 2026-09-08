package platform

// Package memstat reports what one run cost in memory. Heap, not RSS: RSS includes what the
// allocator has not returned to the OS, lags, and counts pages already released, while HeapInuse
// answers how much this program is holding now. Sys is reported beside it.

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// Sample is one reading.
type Sample struct {
	HeapInuse uint64 // bytes in in-use spans
	Sys       uint64 // total obtained from the OS: roughly what RSS will follow
	NumGC     uint32
}

// ReadMemStats takes a reading. It does NOT force a collection: peak matters more than a tidy number.
func ReadMemStats() Sample {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Sample{HeapInuse: m.HeapInuse, Sys: m.Sys, NumGC: m.NumGC}
}

// Delta describes a run: what it started at, ended at, and how much it grew.
type Delta struct {
	Before, After Sample
}

// Growth is the change in in-use heap across the run. Negative when a collection ran.
func (d Delta) Growth() int64 { return int64(d.After.HeapInuse) - int64(d.Before.HeapInuse) }

// String is the one line a daemon logs per tick.
func (d Delta) String() string {
	return fmt.Sprintf("heap %s → %s (%+s), sys %s, gc %d",
		human(d.Before.HeapInuse), human(d.After.HeapInuse), humanSigned(d.Growth()),
		human(d.After.Sys), d.After.NumGC-d.Before.NumGC)
}

func human(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
}

func humanSigned(b int64) string {
	if b < 0 {
		return "-" + human(uint64(-b))
	}
	return human(uint64(b))
}

// --- the soft ceiling ---------------------------------------------------------

// DefaultSoftLimit: raw bytes in flight are capped at 512 MiB across all files and held a few times over
// (staged, scrubbed, sealed, zstd/age buffers), about 2 GiB; the rest is error buffer. Soft, so nearing it costs CPU, but set below what one file needs it GCs the daemon to a standstill.
const DefaultSoftLimit = 3072 << 20

// DefaultMaxInFlightBytes caps concurrent raw bytes at the per-file figure, so the soft limit's derivation holds at any worker count.
const DefaultMaxInFlightBytes = 512 << 20

// EnvMaxInFlightBytes overrides that cap in whole bytes; it exists for the perf tier, production leaves it unset.
const EnvMaxInFlightBytes = "SHIPPER_MAX_IN_FLIGHT_BYTES"

// Written once before any verb runs, then read by the admission gate for the rest of the process.
var maxInFlightBytes int64 = DefaultMaxInFlightBytes

// MaxInFlightBytes is the cap in force for this process.
func MaxInFlightBytes() int64 { return maxInFlightBytes }

// ApplyMaxInFlightBytesFromEnv is the only thing that moves the cap; unset restores the default, a bad value is refused.
func ApplyMaxInFlightBytesFromEnv() error {
	v, ok := os.LookupEnv(EnvMaxInFlightBytes)
	if !ok || strings.TrimSpace(v) == "" {
		maxInFlightBytes = DefaultMaxInFlightBytes
		return nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("%s=%q is not a positive whole number of bytes", EnvMaxInFlightBytes, v)
	}
	maxInFlightBytes = n
	return nil
}

// SoftLimit reports the ceiling in force, or zero when none is held: -1 reads the current value
// without changing it, and Go's unset default reads as MaxInt64, which as a figure would make
// "heap against the limit" read as 0% forever.
func SoftLimit() int64 {
	if l := debug.SetMemoryLimit(-1); l != math.MaxInt64 {
		return l
	}
	return 0
}

// SetSoftLimit makes the GC work harder near limit: soft on purpose, and GOMEMLIMIT wins if the operator set one.
func SetSoftLimit(limit int64) (applied int64, fromEnv bool) {
	// Set AND non-empty: GOMEMLIMIT="" is what a cleared shell variable leaves behind.
	if v, ok := os.LookupEnv("GOMEMLIMIT"); ok && strings.TrimSpace(v) != "" {
		return debug.SetMemoryLimit(-1), true // -1 reads the current value without changing it
	}
	debug.SetMemoryLimit(limit)
	return limit, false
}
