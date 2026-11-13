// Package seal builds and reads the object container:
//
//	tar( manifest.json, <payload> )  ->  zstd level 3  ->  age
//
// manifest.json must stay the FIRST tar entry: every layer decodes sequentially from
// byte 0, so a ranged GET of the head yields the whole manifest without the payload,
// which is why no manifest sidecar exists. The payload itself is never transformed.
package transforms

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Entry names inside the container, fixed rather than derived from the source file: the
// payload's real name is a path, and a path belongs in the manifest only.
const (
	ManifestEntry = "manifest.json"
	PayloadEntry  = "payload"
)

// ZstdLevel is pinned at 3: changing it changes every object's bytes.
const ZstdLevel = 3

// SuggestedPrefixBytes is where a ranged head-fetch should start. It must cover the age
// header, the first zstd block and the first tar entry; age's STREAM chunks decrypt only
// whole, so anything under one 64 KiB chunk plus the header yields nothing at all.
const SuggestedPrefixBytes = 256 * 1024

// maxManifestBytes bounds the manifest read out of an object: a huge one is hostile
// input, not data this code wrote.
const maxManifestBytes = 4 << 20

// maxDecompressedBytes bounds zstd expansion so a crafted object cannot exhaust memory.
const maxDecompressedBytes = 8 << 30

// maxZstdWindow bounds the window a frame may declare, which the decoder allocates up front. Seal
// writes zstd's default 8 MiB window.
const maxZstdWindow = 64 << 20

// ErrPrefixTooShort means the fetched prefix did not contain the whole manifest; the
// caller should double its range rather than treat the object as corrupt.
var ErrPrefixTooShort = errors.New("seal: object prefix too short to contain the manifest")

// Scrubbed proves the payload passed through the scrubber, or a named Unscrubbed decision.
type Scrubbed struct {
	b      []byte
	reader io.Reader
	size   int64
	hash   string
	// via is "scrubber" or the Unscrubbed reason; empty marks the zero value, which Seal refuses.
	via string
}

// Unscrubbed declares raw as shippable without scrubbing; reason says why, e.g. a
// shipper-authored heartbeat or a source configured scrub: false.
func Unscrubbed(raw []byte, reason string) Scrubbed { return Scrubbed{b: raw, via: reason} }

// Bytes returns an in-memory payload, nil for ScrubTo streams; the slice must not be modified.
func (s Scrubbed) Bytes() []byte { return s.b }

// Seal builds one mirror object and returns the manifest as sealed: ShippedHash, PayloadSize
// and, unless the caller set it, Encryption are filled here, so what a caller needs for object
// metadata is the returned copy and never its own.
func Seal(m Manifest, scrubbed Scrubbed, recipients []age.Recipient) ([]byte, Manifest, error) {
	if scrubbed.reader != nil {
		return nil, Manifest{}, errors.New("seal: stream requires SealTo")
	}
	m, err := sealManifest(m, scrubbed, recipients)
	if err != nil {
		return nil, Manifest{}, err
	}
	manifestJSON, err := m.Encode()
	if err != nil {
		return nil, Manifest{}, err
	}
	obj, err := writeContainer(manifestJSON, scrubbed.b, m.PayloadMTime, recipients)
	if err != nil {
		return nil, Manifest{}, err
	}
	return obj, m, nil
}

func sealManifest(m Manifest, scrubbed Scrubbed, recipients []age.Recipient) (Manifest, error) {
	if len(recipients) == 0 {
		return Manifest{}, errors.New("seal: no age recipients: encryption is not optional")
	}
	if scrubbed.via == "" {
		return Manifest{}, errors.New("seal: payload neither scrubbed nor declared Unscrubbed with a reason")
	}
	m.ShippedHash = Hash(scrubbed.b)
	m.PayloadSize = int64(len(scrubbed.b))
	if scrubbed.reader != nil {
		m.ShippedHash, m.PayloadSize = scrubbed.hash, scrubbed.size
	}
	if m.SealedAt == "" {
		return Manifest{}, errors.New("seal: sealed_at must be set by the caller")
	}
	if m.Encryption == nil {
		m.Encryption = &Encryption{Scheme: "age"}
	}
	if len(m.Encryption.RecipientKeyIDs) == 0 {
		for _, r := range recipients {
			m.Encryption.RecipientKeyIDs = append(m.Encryption.RecipientKeyIDs, recipientID(r))
		}
	}

	return m, nil
}

// writeContainer streams all three layers into one pre-sized result buffer, so no
// payload-sized staging copy exists between them.
func writeContainer(
	manifestJSON, payload []byte,
	payloadMTime *time.Time,
	recipients []age.Recipient,
) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, ciphertextHint(len(manifestJSON), len(payload), len(recipients))))
	if err := writeContainerTo(out, manifestJSON, bytes.NewReader(payload), int64(len(payload)), payloadMTime, recipients); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// SealTo preserves the object format while allowing ciphertext to live outside the heap.
func SealTo(w io.Writer, m Manifest, scrubbed Scrubbed, recipients []age.Recipient) (Manifest, error) {
	m, err := sealManifest(m, scrubbed, recipients)
	if err != nil {
		return Manifest{}, err
	}
	manifestJSON, err := m.Encode()
	if err != nil {
		return Manifest{}, err
	}
	r := scrubbed.reader
	if r == nil {
		r = bytes.NewReader(scrubbed.b)
	}
	if err := writeContainerTo(w, manifestJSON, r, m.PayloadSize, m.PayloadMTime, recipients); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func writeContainerTo(w io.Writer, manifestJSON []byte, payload io.Reader, size int64, payloadMTime *time.Time, recipients []age.Recipient) error {
	ageWriter, err := age.Encrypt(w, recipients...)
	if err != nil {
		return fmt.Errorf("seal: age encrypt: %w", err)
	}
	zstdWriter, err := zstd.NewWriter(ageWriter,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(ZstdLevel)),
		zstd.WithEncoderConcurrency(1),
		zstd.WithZeroFrames(true),
	)
	if err != nil {
		return fmt.Errorf("seal: zstd writer: %w", err)
	}
	if err := writeTarReader(zstdWriter, manifestJSON, payload, size, payloadMTime); err != nil {
		return err
	}
	if err := zstdWriter.Close(); err != nil {
		return fmt.Errorf("seal: zstd close: %w", err)
	}
	if err := ageWriter.Close(); err != nil {
		return fmt.Errorf("seal: age close: %w", err)
	}
	return nil
}

func ciphertextHint(manifestLen, payloadLen, recipients int) int {
	const (
		tarBlock  = 512
		zstdBlock = 128 << 10
		ageChunk  = 64 << 10
	)
	round := func(n, block int) int {
		if rem := n % block; rem != 0 {
			return n + block - rem
		}
		return n
	}
	tarLen := 4*tarBlock + round(manifestLen, tarBlock) + round(payloadLen, tarBlock)
	zstdLen := tarLen + 3*(tarLen/zstdBlock+1) + 32
	return zstdLen + 256 + 256*recipients + 16*(zstdLen/ageChunk+1)
}

// writeTarReader writes the two entries, manifest first. Headers are normalised so the tar layer
// contributes no machine-specific bytes; USTAR rather than PAX, whose extended headers
// would sit ahead of the manifest and push it deeper into the object.
func writeTarReader(w io.Writer, manifestJSON []byte, payload io.Reader, size int64, payloadMTime *time.Time) error {
	tw := tar.NewWriter(w)

	mtime := time.Unix(0, 0).UTC()
	if payloadMTime != nil {
		mtime = payloadMTime.UTC().Truncate(time.Second)
	}

	entries := []struct {
		name  string
		body  io.Reader
		size  int64
		mtime time.Time
	}{
		{ManifestEntry, bytes.NewReader(manifestJSON), int64(len(manifestJSON)), time.Unix(0, 0).UTC()},
		{PayloadEntry, payload, size, mtime},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Size:     e.size,
			Mode:     0o600,
			ModTime:  e.mtime,
			Format:   tar.FormatUSTAR,
		}); err != nil {
			return fmt.Errorf("seal: tar header %s: %w", e.name, err)
		}
		var err error
		if body, ok := e.body.(*bytes.Reader); ok {
			_, err = body.WriteTo(tw)
		} else {
			var written int64
			written, err = io.CopyBuffer(tw, io.LimitReader(e.body, e.size), make([]byte, 256<<10))
			if err == nil && written != e.size {
				err = io.ErrUnexpectedEOF
			}
		}
		if err != nil {
			return fmt.Errorf("seal: tar write %s: %w", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("seal: tar close: %w", err)
	}
	return nil
}

// Open decrypts a whole object and returns its manifest and payload.
func Open(object []byte, identities ...age.Identity) (Manifest, []byte, error) {
	var payload bytes.Buffer
	m, err := OpenTo(bytes.NewReader(object), &payload, identities...)
	if err != nil {
		return Manifest{}, nil, err
	}
	return m, payload.Bytes(), nil
}

// OpenTo decrypts an object from r and streams its payload into w, so no payload is held in
// memory. On error w may hold a partial or unverified payload, which the caller must discard.
func OpenTo(r io.Reader, w io.Writer, identities ...age.Identity) (Manifest, error) {
	tr, closeFn, err := tarReader(r, identities...)
	if err != nil {
		return Manifest{}, err
	}
	defer closeFn()

	m, err := readManifestEntry(tr)
	if err != nil {
		return Manifest{}, err
	}

	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("seal: no payload entry: %w", err)
	}
	if hdr.Name != PayloadEntry {
		return Manifest{}, fmt.Errorf("seal: second entry is %q, want %q", hdr.Name, PayloadEntry)
	}
	if hdr.Size != m.PayloadSize || hdr.Size > maxDecompressedBytes {
		return Manifest{}, fmt.Errorf("seal: payload entry is %d bytes, manifest payload_size %d, limit %d",
			hdr.Size, m.PayloadSize, int64(maxDecompressedBytes))
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), tr); err != nil {
		return Manifest{}, fmt.Errorf("seal: read payload: %w", err)
	}

	// The manifest describes bytes; verify it describes these bytes.
	if got := hex.EncodeToString(h.Sum(nil)); got != m.ShippedHash {
		return Manifest{}, fmt.Errorf("seal: payload hash %s does not match manifest shipped_hash %s",
			got, m.ShippedHash)
	}
	return m, nil
}

// ReadManifestPrefix decodes the manifest from a ranged-GET prefix. Reading a prefix
// ALWAYS ends in a truncation error from age or zstd, which must be swallowed once the
// first tar entry is whole; a failure before that is ErrPrefixTooShort instead.
func ReadManifestPrefix(prefix []byte, identities ...age.Identity) (Manifest, error) {
	m, err := ReadManifest(bytes.NewReader(prefix), identities...)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrPrefixTooShort, err)
	}
	return m, nil
}

// ReadManifest decodes the manifest from the head of a whole object, reading no further than
// the manifest needs, so its errors are the real cause rather than truncation.
func ReadManifest(r io.Reader, identities ...age.Identity) (Manifest, error) {
	tr, closeFn, err := tarReader(r, identities...)
	if err != nil {
		return Manifest{}, err
	}
	defer closeFn()
	return readManifestEntry(tr)
}

// readManifestEntry validates that the first tar entry is the manifest; if it is not, the
// container was not built by this code and its layout cannot be trusted.
func readManifestEntry(tr *tar.Reader) (Manifest, error) {
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("read first tar entry: %w", err)
	}
	if hdr.Name != ManifestEntry {
		return Manifest{}, fmt.Errorf("first entry is %q, want %q: manifest-first is the container contract",
			hdr.Name, ManifestEntry)
	}
	if hdr.Size > maxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest claims %d bytes, over the %d limit", hdr.Size, maxManifestBytes)
	}

	raw, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest entry: %w", err)
	}
	if int64(len(raw)) < hdr.Size {
		return Manifest{}, fmt.Errorf("manifest entry truncated: %d of %d bytes", len(raw), hdr.Size)
	}
	return DecodeManifest(raw)
}

// tarReader stacks the three decode layers over r.
func tarReader(r io.Reader, identities ...age.Identity) (*tar.Reader, func(), error) {
	if len(identities) == 0 {
		return nil, nil, errors.New("seal: no age identity supplied")
	}
	dec, err := age.Decrypt(r, identities...)
	if err != nil {
		return nil, nil, fmt.Errorf("age decrypt: %w", err)
	}
	zr, err := zstd.NewReader(dec, zstd.WithDecoderMaxMemory(maxDecompressedBytes), zstd.WithDecoderMaxWindow(maxZstdWindow))
	if err != nil {
		return nil, nil, fmt.Errorf("zstd reader: %w", err)
	}
	return tar.NewReader(zr), zr.Close, nil
}

func recipientID(r age.Recipient) string {
	if s, ok := r.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprintf("%T", r)
}
