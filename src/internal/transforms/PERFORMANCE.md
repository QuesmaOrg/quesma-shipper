# Scrub performance

How this package went from 1.84 to ~72 MB/s (36-41x) on real Claude Code
transcripts with byte-identical redaction, and what must stay true to keep it
there. This file records the numbers, the
architecture, and the approaches that were tried and refuted, so they are not
re-litigated later.

All throughput figures are strictly serial runs of `BenchmarkScrubRealData`
over one frozen 256.7 MiB snapshot of `~/.claude/projects` (131 .jsonl files,
M1 Max). The corpus was snapshotted because the live tree changes mid-run, and
runs were serial because concurrent load once skewed a measurement by 40%.

## Throughput by stage

| Stage (commit) | MB/s | vs baseline | Allocated per pass |
|---|---:|---:|---|
| baseline (e3d3f61) | 1.84 | 1.0x | 10.1 GB / 28.5M allocs |
| + allocation diet on the JSONL walk (4a10b3c) | 2.25 | 1.2x | 3.3 GB / 8.7M |
| + one keyword automaton for the rule ladder (14db7a4) | 6.7 | 3.6x | 3.0 GB / 7.7M |
| + the ladder stops running regexes it does not need (8275fcf) | 28.5 | 15.5x | 3.0 GB / 7.7M |
| + hand JSON tokenizer, fused PII pass, entropy scoring (6a85aee) | 66.9-78.8 | 36-41x | 1.74 GB / 1.34M |

## Why it was slow

The baseline profile was 75% regexp execution. Three compounding causes:

- The keyword prefilter (`containsAny`) lowercased the whole value once per
  rule, about 29 times per value.
- The key-name matcher ran a 19-way `(?i)` alternation over every value with
  no prefilter: 34% of total CPU by itself.
- Go's `regexp` guarantees linear time but runs a Pike VM: cost per byte is
  proportional to the number of live NFA states. It has no lazy DFA, which is
  the machine that makes C++ RE2, Rust's regex and Hyperscan fast.

## How the fix works

One precomputed Aho-Corasick DFA over every rule's ASCII-folded keywords plus
the key-name stems scans each value once (`packs/prefilter.go`); rules whose
gate did not fire never run. Rules that do fire mostly avoid the regexp engine
anyway: card-pan, pesel, iban and email are hand byte-scanners
(`packs/pii.go`, `packs/handscan.go`), and the remaining rules are matched by
literal anchoring (`packs/anchor.go`): memchr to occurrences of the corpus
keywords, confirm with a `\A`-anchored regex. Dispatch order is hand scanner,
then anchored, then linear sweep. (Amended 2026-08-17: anchoring originally
derived the required head literal from the `regexp/syntax` tree; the
derivation was replaced by the corpus `keywords` field, with the four rules
whose keywords are not match heads declaring `"sweep": true`.)
Around the matchers: the JSONL walk parses its own JSON with exact
encoding/json v1 acceptance (`jsonwalk.go`), objects are parallel slices on a
node arena rather than maps, strings re-serialize through a hand escaper, and
entropy scoring uses a narrow histogram with precomputed log tables.

Keyword matching folds ASCII letter bytes and nothing else. (Amended
2026-08-17: the DFA originally folded the two runes Unicode lowercases into
ASCII, U+212A KELVIN SIGN and U+0130, in-scan, and U+017F widened the
key-name gate; review judged the corner unrealistic and the handling was
removed as an accepted narrowing.)

## Where the CPU goes now

Profile of the final stage on the real corpus (78.8 MB/s run):

| Share | Component |
|---:|---|
| 23% | the Aho-Corasick DFA scan itself (~370 MB/s through the automaton) |
| 21% | gated rule verification: hand scanners, anchored confirms, residual regexp (~9%) |
| 18% | entropy candidate scoring |
| 16% | GC and copies |
| 10% | memchr (line splitting, anchor candidate hunts) |
| 10% | the hand JSON parse and the fused PII walk |

No single dominant target remains. Known headroom, each worth maybe 5-15%
end to end and deliberately left for separate changes: `visitStrings`
child-path concatenation (about 41% of remaining allocations),
`splitKeyWords` per-key allocations, memchr skip loops inside the DFA scan,
and fusing the entropy candidate walk into the PII classification pass.

## Tried and refuted

Do not re-propose these without new evidence.

- **One merged alternation regex.** Measured 1.38 MB/s against 2.24 for
  separate regexes and 7.78 for the prefiltered ladder. The Pike VM pays per
  live NFA state per byte; a 28-way union keeps most branches alive at every
  position, forfeits the literal-prefix skip, and disqualifies the fast
  small-pattern engines. The one-scan idea won one layer down instead: the
  automaton is the single DFA pass, built over literals where a DFA is cheap.
- **ASCII-only case folding in the prefilter.** A reviewer constructed a
  concrete missed secret via U+212A folding into the k of "token". The DFA
  folds the two dangerous runes in-scan instead.
- **Window-scoped regex execution.** Superseded by literal anchoring, which
  reaches the same goal with a per-rule soundness derivation instead of
  window-size heuristics.
- **sync.Pool scratch reuse.** Redaction-identical but its arena release
  cleared the whole retained slab on every call: a verified 5x regression once
  one wide record had grown the arena. The JSON tokenizer captured most of the
  allocation win without the hazard.
- **Anchoring `private-key-block`.** Its unbounded lazy tail made candidate
  verification quadratic (up to 481x on a flood of unterminated PEM headers).
  The anchor derivation refuses unbounded-tail patterns; that rule keeps the
  linear sweep, and a regression test pins the decision.

## Verification record

- Differential replay against the previous stage and against baseline at every
  stage, comparing output bytes and the full ledger (BytesRedacted, ScanMode,
  LinesParsed, LinesRawScanned, RuleHits), each output re-scrubbed once for
  idempotency: 110k, 190k and 671k end-to-end pairs at the three composition
  points, zero divergence, all 27 rule ids exercised.
- Rule-level exhaustive sweeps: 62.7M values for the card-pan chain
  derivation, ~9.7M for email, 31M-execution fuzz on the JSON tokenizer's
  acceptance set, plus real-transcript replays of 916 MB, 1.7 GB and 1.51 GB
  in the independent reviews.
- Harnesses were mutation-tested (8 of 9 injected scanner bugs caught, the
  ninth provably equivalent) and negative-controlled (two deliberate sabotages
  initially escaped; the corpora were strengthened until both were caught).
- `make check` and `go test -race` green at every commit.

## Standing invariants

- Every fast path keeps its oracle in-repo: the stdlib decoder loop, the
  reference regexes and the sweep matcher survive as test-only references with
  differential tests. Deleting one of those tests removes the only thing
  holding a fast path to its specification.
- JSON acceptance parity is pinned to encoding/json v1. A GOEXPERIMENT=jsonv2
  toolchain fails `jsonwalk_test.go` loudly rather than drifting; that is a
  decision to make, not a bug to chase.
- The anchor derivation table is pinned per rule, so a corpus edit that
  changes a rule's execution path shows up as a visible test diff.
- Config surface: negative `EntropyConfig.MinLength` and empty or non-ASCII
  corpus keywords are loud construction errors. Non-ASCII configured key names
  keep working exactly as before (the key-name gate falls back to always-run).

## Reproducing

```sh
# real-data throughput; skips when the variable is unset, so CI never touches
# private data
SCRUB_BENCH_DIR=$HOME/.claude/projects \
  go test ./internal/transforms/ -bench BenchmarkScrubRealData -benchmem -run '^$' -benchtime 1x

# portable synthetic proxy
go test ./internal/transforms/ -bench BenchmarkScrubSynthetic -benchmem -run '^$'
```

Measurement rules that made the numbers comparable: freeze the corpus before
comparing anything, run benchmarks strictly serially, and interleave old/new
runs when A/B-ing so machine drift shows up as spread rather than as a result.
