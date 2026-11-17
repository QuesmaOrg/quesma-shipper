# scrub

The scrubber: what removes credentials and personal data from a payload before it is sealed. The
shipper runs it on every file it ships. It is a Go module of its own, with no dependency, so a
component outside the shipper can scrub with the same rules.

```go
import "github.com/QuesmaOrg/quesma-shipper/scrub"

s, err := scrub.New(scrub.DefaultConfig())
res, err := s.Scrub(payload, scrub.Hint{Family: "claude-code", JSONL: true})
clean := res.Out.Bytes()
```

- `Scrub` takes a whole payload; `ScrubTo` streams one that does not fit in memory.
- `Hint.JSONL` walks each line as JSON, so a field's name can decide what its value is. Anything
  else is scanned as text. Opaque binary is refused: a scrub error means the payload does not ship.
- `Hint.Family` selects the structural exemptions and field drops written for one agent's format.
- `Result.Out` is a `Scrubbed`: proof for a sealer that the payload passed through here, or through
  `Unscrubbed` with a stated reason.
- `packs/` holds the compiled rule packs. A config can add packs; it cannot remove a built-in.

The conformance vectors are in [`src/conformance/v1/scrub`](../src/conformance/v1/scrub): before
and after pairs that pin the output byte for byte. A diff there is a claim the output should
change. How it got fast, and what keeps it there: [PERFORMANCE.md](PERFORMANCE.md).

`make test`, `make vet` and `make race` at the repository root run this module with the shipper's.
