// Package sqliteread reads live agent SQLite databases as ENRICHER INPUT ONLY; no rows ever ship. Three read methods,
// tried in order and recorded in the manifest: read-only in-place open, VACUUM INTO scratch snapshot, cold raw
// copy of db plus -wal/-shm. It never opens the original read-write and never byte-copies a live database.
package sqliteread

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ReadMethod names how a read was obtained. Recorded in the manifest's db_provenance.
type ReadMethod string

// The method names are what manifest.schema.json enumerates; a value it does not list makes every derived object unshippable.
const (
	// ReadInPlace is the fast path: the live file, opened read-only.
	ReadInPlace ReadMethod = "readonly_open"

	// ReadSnapshot is a consistent copy made with VACUUM INTO, queried and deleted.
	ReadSnapshot ReadMethod = "scratch_snapshot"

	// ReadColdCopy is a raw copy of a database nothing is writing, sidecars included.
	ReadColdCopy ReadMethod = "cold_copy"
)

// coldAfter is how long a database must be untouched to count as cold. Generous: being wrong means copying a live database.
const coldAfter = 5 * time.Minute

// Row is one key/value pair, already filtered.
type Row struct {
	Key   string
	Value []byte
}

// Result is what a read produced.
type Result struct {
	Rows   []Row
	Method ReadMethod

	// Truncated says the row cap stopped the read. A flag, not an error: an error would send the read to the next fallback.
	Truncated bool

	// DeniedKeys and StrippedFields count what the compiled filter removed, so the filter's operation is observable rather than assumed.
	DeniedKeys     int
	StrippedFields int
}

// Options configures a read.
type Options struct {
	// Path is the database. Never opened read-write.
	Path string

	// ScratchDir is where a snapshot read puts its copy: under the state directory, never beside the source.
	ScratchDir string

	// Table and KeyPrefixes are the read's DECLARED scope, named by the enricher at registration, not discovered here.
	Table       string
	KeyPrefixes []string

	// MaxRows bounds a read; the cap is reported rather than silently applied.
	MaxRows int

	// Now is injectable for the coldness check.
	Now func() time.Time

	// StartAt skips the methods before it, so doctor and the tests can exercise a fallback before the day it is needed. Empty starts at the top.
	StartAt ReadMethod
}

// ErrTruncated means MaxRows cut the read short.
var ErrTruncated = errors.New("sqliteread: row cap reached")

// errSkipped reports a method StartAt skipped, so a failure message cannot imply it was tried.
var errSkipped = errors.New("skipped by StartAt")

// Read tries each method in order; falling back is only for a method that CANNOT READ the database: a row-cap hit is not that, so it stops here.
func Read(o Options) (Result, error) {
	if o.Path == "" {
		return Result{}, errors.New("sqliteread: no database path")
	}
	if o.Table == "" {
		return Result{}, errors.New("sqliteread: no table declared: an enricher's scope is declared, not discovered")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxRows <= 0 {
		o.MaxRows = 200_000
	}

	inPlaceErr, snapshotErr := errSkipped, errSkipped

	// First: in place.
	if o.StartAt == "" || o.StartAt == ReadInPlace {
		res, err := readInPlace(o)
		if err == nil {
			return res, nil
		}
		inPlaceErr = err
	}

	// Second: snapshot. A snapshot is consistent where a byte copy is not, so it beats a raw copy even on a cold database.
	if o.StartAt != ReadColdCopy {
		res, err := readSnapshot(o)
		if err == nil {
			return res, nil
		}
		snapshotErr = err
	}

	// Last: raw copy, cold only.
	res, err := readColdCopy(o)
	if err == nil {
		return res, nil
	}
	return Result{}, fmt.Errorf("sqliteread: every read method failed for %s:\n  in-place: %v\n  snapshot: %v\n  cold copy: %v",
		o.Path, inPlaceErr, snapshotErr, err)
}

func finish(db *sql.DB, o Options, method ReadMethod) (Result, error) {
	res, err := query(db, o)
	if err != nil && !errors.Is(err, ErrTruncated) {
		return Result{}, err
	}
	res.Truncated = errors.Is(err, ErrTruncated)
	res.Method = method
	return res, nil
}

// readInPlace is the fast path: mode=ro and deliberately NOT immutable=1, which would skip the WAL and silently return stale data.
func readInPlace(o Options) (Result, error) {
	// busy_timeout: meeting a writer's lock is the ordinary case here, and failing instantly costs a whole VACUUM copy.
	dsn := "file:" + o.Path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)&_txlock=deferred"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	return finish(db, o, ReadInPlace)
}

// readSnapshot is the second method: VACUUM INTO a scratch file, query it, delete it. A leftover is a corrupt partial: delete it, never resume into it.
func readSnapshot(o Options) (result Result, err error) {
	if o.ScratchDir == "" {
		return Result{}, errors.New("sqliteread: no scratch directory for a snapshot read")
	}
	if err := os.MkdirAll(o.ScratchDir, 0o700); err != nil {
		return Result{}, err
	}

	scratch := filepath.Join(o.ScratchDir, "snapshot-"+sanitise(filepath.Base(o.Path))+".sqlite")
	// Delete any leftover FIRST: a file from a crashed run is a corrupt partial that would query as if valid.
	if err := removeSnapshot(scratch); err != nil {
		return Result{}, err
	}
	defer func() {
		if rmErr := removeSnapshot(scratch); rmErr != nil && err == nil {
			// A snapshot left behind is a copy of an agent's database in the state directory; worth failing the read over.
			err = fmt.Errorf("sqliteread: snapshot read succeeded but %s could not be removed: %w",
				scratch, rmErr)
			result = Result{}
		}
	}()

	src, err := sql.Open("sqlite", "file:"+o.Path+"?mode=ro")
	if err != nil {
		return Result{}, err
	}
	defer src.Close()

	// Quoted as a SQL string literal: VACUUM INTO takes an expression, and a path may contain a quote.
	if _, err := src.Exec("VACUUM INTO " + quoteSQLString(scratch)); err != nil {
		return Result{}, fmt.Errorf("vacuum into %s: %w", scratch, err)
	}

	snap, err := sql.Open("sqlite", "file:"+scratch+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return Result{}, err
	}
	defer snap.Close()

	return finish(snap, o, ReadSnapshot)
}

// readColdCopy is the last resort: copy db, -wal and -shm together, then open the copy read-only. Cold databases only,
// and the sidecars must keep MATCHING basenames, or the copy opens without the WAL and silently shows data
// from before the last checkpoint.
func readColdCopy(o Options) (result Result, err error) {
	if o.ScratchDir == "" {
		return Result{}, errors.New("sqliteread: no scratch directory for a cold copy")
	}
	info, err := os.Stat(o.Path)
	if err != nil {
		return Result{}, err
	}
	if age := o.Now().Sub(info.ModTime()); age < coldAfter {
		return Result{}, fmt.Errorf("database was modified %s ago, which is not cold enough for a raw copy "+
			"(a raw copy of a live database is not consistent)", age.Round(time.Second))
	}

	dir := filepath.Join(o.ScratchDir, "cold-"+sanitise(filepath.Base(o.Path)))
	if err := os.RemoveAll(dir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, err
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("sqliteread: cold copy succeeded but %s could not be removed: %w", dir, rmErr)
			result = Result{}
		}
	}()

	copied, err := CopyCold(o.Path, dir)
	if err != nil {
		return Result{}, err
	}

	db, err := sql.Open("sqlite", "file:"+copied+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	return finish(db, o, ReadColdCopy)
}

// ErrSidecarBasename means a copy would have renamed a WAL sidecar.
var ErrSidecarBasename = errors.New("sqliteread: a copied database and its -wal/-shm sidecars must keep matching basenames")

// CopyCold copies a database and its sidecars into dir, keeping basenames. Returns the copied database's path.
func CopyCold(src, dir string) (string, error) {
	base := filepath.Base(src)
	dst := filepath.Join(dir, base)

	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := src + suffix
		if _, err := os.Stat(sidecar); err != nil {
			// Absent is fine: a checkpointed database has no -wal.
			continue
		}
		want := dst + suffix
		if filepath.Base(want) != base+suffix {
			// Unreachable by construction: the invariant is asserted where a future refactor would break it.
			return "", fmt.Errorf("%w: %s would become %s", ErrSidecarBasename, sidecar, want)
		}
		if err := copyFile(sidecar, want); err != nil {
			return "", err
		}
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	// Read-only open of the source. This package never opens an agent's file for writing.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// query runs the declared read and applies the compiled filter.
func query(db *sql.DB, o Options) (Result, error) {
	var res Result

	// Prefix parameters are bound, never interpolated, so a prefix from anywhere else cannot rewrite the query.
	sb := &strings.Builder{}
	fmt.Fprintf(sb, "SELECT key, value FROM %s", quoteIdent(o.Table))
	var args []any
	if len(o.KeyPrefixes) > 0 {
		sb.WriteString(" WHERE ")
		for i, p := range o.KeyPrefixes {
			if i > 0 {
				sb.WriteString(" OR ")
			}
			sb.WriteString("key LIKE ? ESCAPE '\\'")
			args = append(args, escapeLike(p)+"%")
		}
	}
	// Ordered, so a read is reproducible and an enricher's output does not depend on SQLite's row order.
	sb.WriteString(" ORDER BY key")

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()

	for rows.Next() {
		if len(res.Rows) >= o.MaxRows {
			return res, ErrTruncated
		}
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return Result{}, err
		}

		// The compiled filter, applied to every row of every read: auth material lives in the same database as the trajectories.
		if keyDenied(key) {
			res.DeniedKeys++
			continue
		}
		cleaned, stripped := scrubValue(value)
		res.StrippedFields += stripped
		if cleaned == nil {
			// Dropping the row is the only outcome that cannot leak the field.
			res.DeniedKeys++
			continue
		}
		res.Rows = append(res.Rows, Row{Key: key, Value: cleaned})
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	return res, nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// escapeLike neutralises LIKE wildcards, so a declared prefix cannot sweep in neighbouring keyspaces.
func escapeLike(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func sanitise(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// removeSnapshot deletes a snapshot and its sidecars.
func removeSnapshot(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
