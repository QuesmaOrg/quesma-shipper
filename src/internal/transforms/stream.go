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

// StreamRecordThreshold serializes exceptional records before their buffers grow further.
const StreamRecordThreshold = 64 << 20

var largeRecordSlot = make(chan struct{}, 1)

const streamBatchBytes = 1 << 20

// ScrubTo spools only scrubbed JSONL, retaining exact line endings and the whole-file ledger.
func (s *Scrubber) ScrubTo(ctx context.Context, dst io.ReadWriteSeeker, src io.Reader, hint Hint, maxBytes int64) (Result, string, error) {
	return s.scrubTo(ctx, dst, src, hint, maxBytes, StreamRecordThreshold, largeRecordSlot)
}

func (s *Scrubber) scrubTo(ctx context.Context, dst io.ReadWriteSeeker, src io.Reader, hint Hint, maxBytes int64, recordThreshold int, slot chan struct{}) (Result, string, error) {
	if !hint.JSONL {
		return Result{}, "", errors.New("stream scrub requires JSONL")
	}
	rawHash, shippedHash := sha256.New(), sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(src, rawHash), 64<<10)
	writer := io.MultiWriter(dst, shippedHash)
	result := Result{RuleHits: map[string]int{}, ScanMode: ScanModeDecodedJSON}
	batch := make([]byte, 0, streamBatchBytes)
	var recordBytes int
	var size, bytesRead int64
	first, exceptional := true, false
	defer func() {
		if exceptional {
			<-slot
		}
	}()
	flush := func(payload []byte) error {
		if len(payload) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := s.scrub(payload, hint, first)
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
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, "", err
		}
		part, err := reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
			return Result{}, "", err
		}
		bytesRead += int64(len(part))
		if maxBytes > 0 && bytesRead > maxBytes {
			return Result{}, "", fmt.Errorf("decoded size exceeds max_file_bytes %d", maxBytes)
		}
		if !exceptional && recordBytes+len(part) > recordThreshold {
			if maxBytes <= 0 {
				return Result{}, "", errors.New("oversized JSONL record requires a positive max_file_bytes cap")
			}
			// Completed records must not share the exceptional record's scrub buffer.
			start := len(batch) - recordBytes
			if e := flush(batch[:start]); e != nil {
				return Result{}, "", e
			}
			batch = batch[start:]
			select {
			case slot <- struct{}{}:
				exceptional = true
			case <-ctx.Done():
				return Result{}, "", ctx.Err()
			}
			if e := ctx.Err(); e != nil {
				return Result{}, "", e
			}
		}
		recordBytes += len(part)
		batch = append(batch, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		recordBytes = 0
		if exceptional || len(batch) >= streamBatchBytes || errors.Is(err, io.EOF) {
			if e := flush(batch); e != nil {
				return Result{}, "", e
			}
			if exceptional {
				batch = make([]byte, 0, streamBatchBytes)
				<-slot
				exceptional = false
			} else {
				batch = batch[:0]
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
