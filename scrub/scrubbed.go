package scrub

import "io"

// UserPlaceholder is what the shipper writes in place of the OS user's name; the scrubber never
// treats it as a finding. The same string as formats.UserPlaceholder in the shipper.
const UserPlaceholder = "__USER__"

// MaxPayloadBytes is the room a sealed container has for a payload: its 8 GiB limit, less the
// manifest's 4 MiB and the tar framing. ScrubTo refuses to write past it.
const MaxPayloadBytes int64 = 8<<30 - 4<<20 - 4096

// Scrubbed proves the payload passed through the scrubber, or a named Unscrubbed decision.
type Scrubbed struct {
	b      []byte
	reader io.Reader
	size   int64
	hash   string
	// via is "scrubber" or the Unscrubbed reason; empty marks the zero value, which a sealer refuses.
	via string
}

// Unscrubbed declares raw as shippable without scrubbing; reason says why, e.g. a
// shipper-authored heartbeat or a source configured scrub: false.
func Unscrubbed(raw []byte, reason string) Scrubbed { return Scrubbed{b: raw, via: reason} }

// Bytes returns an in-memory payload, nil for ScrubTo streams; the slice must not be modified.
func (s Scrubbed) Bytes() []byte { return s.b }

// Via is "scrubber", the Unscrubbed reason, or empty for the zero value.
func (s Scrubbed) Via() string { return s.via }

// Stream is a ScrubTo payload with its size and SHA-256; the reader is nil for an in-memory one.
func (s Scrubbed) Stream() (r io.Reader, size int64, hash string) { return s.reader, s.size, s.hash }
