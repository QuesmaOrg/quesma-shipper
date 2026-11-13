package sources

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// StreamThreshold keeps ordinary files on the allocation-efficient in-memory path.
const StreamThreshold = 8 << 20

func streamablePath(path string) bool {
	return strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".jsonl.zst")
}

type Stream struct {
	io.ReadCloser
	MTime time.Time
}

func fileStream(path string, maxBytes int64) func(context.Context) (Stream, error) {
	return func(ctx context.Context) (Stream, error) {
		f, info, err := platform.Open(path)
		if err != nil {
			return Stream{}, err
		}
		var r io.Reader = &snapshotReader{reader: io.NewSectionReader(f, 0, info.Size()), remaining: info.Size()}
		close := f.Close
		if IsZstd(path) {
			dec, _, err := newZstdReader(r, path)
			if err != nil {
				_ = f.Close()
				return Stream{}, err
			}
			r = dec
			close = func() error { dec.Close(); return f.Close() }
		} else if maxBytes > 0 && info.Size() > maxBytes {
			_ = f.Close()
			return Stream{}, fmt.Errorf("%w: file exceeds max_file_bytes %d", platform.ErrTooLarge, maxBytes)
		}
		return Stream{ReadCloser: &boundedStream{reader: r, close: close, ctx: ctx, remaining: maxBytes, limited: maxBytes > 0}, MTime: info.ModTime()}, nil
	}
}

type boundedStream struct {
	reader    io.Reader
	close     func() error
	ctx       context.Context
	remaining int64
	limited   bool
}

func (r *boundedStream) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if !r.limited {
		return r.reader.Read(p)
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: decoded size exceeds max_file_bytes", platform.ErrTooLarge)
		}
		return 0, err
	}
	p = p[:min(int64(len(p)), r.remaining)]
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}
func (r *boundedStream) Close() error { return r.close() }

// A shrink during the snapshot must not turn a partial read into a successful upload.
type snapshotReader struct {
	reader    io.Reader
	remaining int64
}

func (r *snapshotReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
