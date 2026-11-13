package transforms

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// MaxStreamRecordBytes bounds the existing JSON scrubber's working set, independent of session length.
const MaxStreamRecordBytes = 64 << 20
const streamBatchBytes = 1 << 20

// ScrubTo spools only scrubbed JSONL, retaining exact line endings and the whole-file ledger.
func (s *Scrubber) ScrubTo(ctx context.Context, dst io.ReadWriteSeeker, src io.Reader, hint Hint) (Result, string, error) {
	if !hint.JSONL {
		return Result{}, "", errors.New("stream scrub requires JSONL")
	}
	rawHash, shippedHash := sha256.New(), sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(src, rawHash), 64<<10)
	writer := io.MultiWriter(dst, shippedHash)
	result := Result{RuleHits: map[string]int{}, ScanMode: ScanModeDecodedJSON}
	batch := make([]byte, 0, streamBatchBytes)
	var recordBytes int
	var size int64
	first := true
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		r, err := s.scrub(batch, hint, first)
		if err != nil {
			return err
		}
		first = false
		body := r.Out.Bytes()
		if int64(len(body)) > maxDecompressedBytes-maxManifestBytes-4096-size {
			return errors.New("scrubbed payload exceeds the existing 8 GiB container limit")
		}
		n, err := writer.Write(body)
		if err != nil {
			return err
		}
		if n != len(body) {
			return io.ErrShortWrite
		}
		size += int64(n)
		result.BytesTotal += r.BytesTotal
		result.record(r.BytesRedacted, r.RuleHits)
		result.LinesParsed += r.LinesParsed
		result.LinesRawScanned += r.LinesRawScanned
		batch = batch[:0]
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, "", err
		}
		part, err := reader.ReadSlice('\n')
		recordBytes += len(part)
		if recordBytes > MaxStreamRecordBytes {
			return Result{}, "", fmt.Errorf("JSONL record exceeds the %d-byte streaming limit", MaxStreamRecordBytes)
		}
		batch = append(batch, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return Result{}, "", err
		}
		recordBytes = 0
		if len(batch) >= streamBatchBytes || errors.Is(err, io.EOF) {
			if e := flush(); e != nil {
				return Result{}, "", e
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	switch {
	case result.LinesParsed > 0 && result.LinesRawScanned > 0:
		result.ScanMode = ScanModeMixed
	case result.LinesRawScanned > 0:
		result.ScanMode = ScanModeRawText
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return Result{}, "", err
	}
	result.Out = Scrubbed{reader: dst, size: size, hash: hex.EncodeToString(shippedHash.Sum(nil)), via: viaScrubber}
	return result, hex.EncodeToString(rawHash.Sum(nil)), nil
}
