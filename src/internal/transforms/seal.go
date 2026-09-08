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

// ErrPrefixTooShort means the fetched prefix did not contain the whole manifest; the
// caller should double its range rather than treat the object as corrupt.
var ErrPrefixTooShort = errors.New("seal: object prefix too short to contain the manifest")

// Seal builds one mirror object. It computes ShippedHash and PayloadSize; a ShippedHash
// the caller already set is VERIFIED rather than overwritten, since a caller that hashed
// different bytes than it passed is a defect worth catching. The manifest is taken by
// VALUE, so a caller needing these fields (for object metadata) must set them itself.
func Seal(m Manifest, payload []byte, recipients []age.Recipient) ([]byte, error) {
	if len(recipients) == 0 {
		// An object with no recipient is either unreadable or unencrypted.
		return nil, errors.New("seal: no age recipients: encryption is not optional")
	}

	shipped := Hash(payload)
	if m.ShippedHash != "" && m.ShippedHash != shipped {
		return nil, fmt.Errorf("seal: manifest shipped_hash %s does not match the payload's %s: "+
			"the caller hashed different bytes than it passed", m.ShippedHash, shipped)
	}
	m.ShippedHash = shipped
	m.PayloadSize = int64(len(payload))
	if m.SealedAt == "" {
		return nil, errors.New("seal: sealed_at must be set by the caller")
	}
	if m.Encryption == nil {
		m.Encryption = &Encryption{Scheme: "age"}
	}
	if len(m.Encryption.RecipientKeyIDs) == 0 {
		for _, r := range recipients {
			m.Encryption.RecipientKeyIDs = append(m.Encryption.RecipientKeyIDs, recipientID(r))
		}
	}

	manifestJSON, err := m.Encode()
	if err != nil {
		return nil, err
	}

	return writeContainer(manifestJSON, payload, m.PayloadMTime, recipients)
}

// writeContainer streams all three layers into one pre-sized result buffer, so no
// payload-sized staging copy exists between them.
func writeContainer(
	manifestJSON, payload []byte,
	payloadMTime *time.Time,
	recipients []age.Recipient,
) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, ciphertextHint(len(manifestJSON), len(payload), len(recipients))))
	ageWriter, err := age.Encrypt(out, recipients...)
	if err != nil {
		return nil, fmt.Errorf("seal: age encrypt: %w", err)
	}
	zstdWriter, err := zstd.NewWriter(ageWriter,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(ZstdLevel)),
		zstd.WithEncoderConcurrency(1),
		zstd.WithZeroFrames(true),
	)
	if err != nil {
		return nil, fmt.Errorf("seal: zstd writer: %w", err)
	}
	if err := writeTar(zstdWriter, manifestJSON, payload, payloadMTime); err != nil {
		return nil, err
	}
	if err := zstdWriter.Close(); err != nil {
		return nil, fmt.Errorf("seal: zstd close: %w", err)
	}
	if err := ageWriter.Close(); err != nil {
		return nil, fmt.Errorf("seal: age close: %w", err)
	}
	return out.Bytes(), nil
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

// writeTar writes the two entries, manifest first. Headers are normalised so the tar layer
// contributes no machine-specific bytes; USTAR rather than PAX, whose extended headers
// would sit ahead of the manifest and push it deeper into the object.
func writeTar(w io.Writer, manifestJSON, payload []byte, payloadMTime *time.Time) error {
	tw := tar.NewWriter(w)

	mtime := time.Unix(0, 0).UTC()
	if payloadMTime != nil {
		mtime = payloadMTime.UTC().Truncate(time.Second)
	}

	entries := []struct {
		name  string
		body  []byte
		mtime time.Time
	}{
		{ManifestEntry, manifestJSON, time.Unix(0, 0).UTC()},
		{PayloadEntry, payload, mtime},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Size:     int64(len(e.body)),
			Mode:     0o600,
			ModTime:  e.mtime,
			Format:   tar.FormatUSTAR,
		}); err != nil {
			return fmt.Errorf("seal: tar header %s: %w", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
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
	tr, closeFn, err := tarReader(bytes.NewReader(object), identities...)
	if err != nil {
		return Manifest{}, nil, err
	}
	defer closeFn()

	m, err := readManifestEntry(tr)
	if err != nil {
		return Manifest{}, nil, err
	}

	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("seal: no payload entry: %w", err)
	}
	if hdr.Name != PayloadEntry {
		return Manifest{}, nil, fmt.Errorf("seal: second entry is %q, want %q", hdr.Name, PayloadEntry)
	}
	payload, err := io.ReadAll(io.LimitReader(tr, maxDecompressedBytes))
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("seal: read payload: %w", err)
	}

	// The manifest describes bytes; verify it describes these bytes.
	if got := Hash(payload); got != m.ShippedHash {
		return Manifest{}, nil, fmt.Errorf("seal: payload hash %s does not match manifest shipped_hash %s",
			got, m.ShippedHash)
	}
	return m, payload, nil
}

// ReadManifestPrefix decodes the manifest from a ranged-GET prefix. Reading a prefix
// ALWAYS ends in a truncation error from age or zstd, which must be swallowed once the
// first tar entry is whole; a failure before that is ErrPrefixTooShort instead.
func ReadManifestPrefix(prefix []byte, identities ...age.Identity) (Manifest, error) {
	tr, closeFn, err := tarReader(bytes.NewReader(prefix), identities...)
	if err != nil {
		// A prefix too short to hold even the age header fails here.
		return Manifest{}, fmt.Errorf("%w: %v", ErrPrefixTooShort, err)
	}
	defer closeFn()

	m, err := readManifestEntry(tr)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrPrefixTooShort, err)
	}
	return m, nil
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
	zr, err := zstd.NewReader(dec, zstd.WithDecoderMaxMemory(maxDecompressedBytes))
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
