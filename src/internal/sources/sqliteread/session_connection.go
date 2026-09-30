package sqliteread

// Session connections verify SQLite's open database and WAL handles against an anchored directory.
// Native handle access is confined here because database/sql does not expose SQLite file controls.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

type sessionPath struct {
	parent     *os.Root
	path, name string
	info       os.FileInfo
}

func anchorSessionPath(path string) (*sessionPath, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("session database path must be absolute")
	}
	volume := filepath.VolumeName(path)
	root, err := os.OpenRoot(volume + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), volume+string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !before.IsDir() {
			root.Close()
			return nil, fmt.Errorf("session path component is not a directory: %s", part)
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, fmt.Errorf("session directory changed while opening: %s", part)
		}
		root = next
	}
	name := parts[len(parts)-1]
	info, err := root.Lstat(name)
	if err != nil {
		root.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		root.Close()
		return nil, fmt.Errorf("session database is not a regular file")
	}
	return &sessionPath{parent: root, path: path, name: name, info: info}, nil
}

type sessionConnection struct {
	tls   *libc.TLS
	db    uintptr
	bound *sessionPath
}

func openSessions(ctx context.Context, path string) (*sessionConnection, error) {
	bound, err := anchorSessionPath(path)
	if err != nil {
		return nil, err
	}
	return bound.open(ctx)
}

func (p *sessionPath) open(ctx context.Context) (_ *sessionConnection, err error) {
	c := &sessionConnection{tls: libc.NewTLS(), bound: p}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	name, err := libc.CString(sessionFileURI(filepath.ToSlash(p.path)))
	if err != nil {
		return nil, err
	}
	defer libc.Xfree(c.tls, name)
	vfs, err := libc.CString(sessionVFS)
	if err != nil {
		return nil, err
	}
	defer libc.Xfree(c.tls, vfs)
	ptr := c.tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer c.tls.Free(int(unsafe.Sizeof(uintptr(0))))
	rc := sqlite3.Xsqlite3_open_v2(c.tls, name, ptr, sqlite3.SQLITE_OPEN_READONLY|sqlite3.SQLITE_OPEN_URI|sqlite3.SQLITE_OPEN_FULLMUTEX|sqlite3.SQLITE_OPEN_NOFOLLOW, vfs)
	c.db = nativeValue[uintptr](ptr)
	if rc != sqlite3.SQLITE_OK {
		return nil, c.err(rc)
	}
	if err = c.verifyFiles(false); err != nil {
		return nil, err
	}
	if rc = sqlite3.Xsqlite3_busy_timeout(c.tls, c.db, 2000); rc != sqlite3.SQLITE_OK {
		return nil, c.err(rc)
	}
	for _, q := range []string{"PRAGMA query_only=ON", "BEGIN", "SELECT count(*) FROM sqlite_schema"} {
		if err = c.query(ctx, q, nil, nil); err != nil {
			return nil, err
		}
	}
	if err = c.verifyFiles(true); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *sessionConnection) Close() {
	if c.db != 0 {
		sqlite3.Xsqlite3_close_v2(c.tls, c.db)
	}
	c.tls.Close()
	c.bound.parent.Close()
}

func (c *sessionConnection) err(rc int32) error {
	return fmt.Errorf("session sqlite (%d): %s", rc, libc.GoString(sqlite3.Xsqlite3_errmsg(c.tls, c.db)))
}

func (c *sessionConnection) filePointer(op int32) (uintptr, error) {
	name, err := libc.CString("main")
	if err != nil {
		return 0, err
	}
	defer libc.Xfree(c.tls, name)
	p := c.tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer c.tls.Free(int(unsafe.Sizeof(uintptr(0))))
	clear(libc.GoBytes(p, int(unsafe.Sizeof(uintptr(0)))))
	if rc := sqlite3.Xsqlite3_file_control(c.tls, c.db, name, op, p); rc != sqlite3.SQLITE_OK {
		return 0, c.err(rc)
	}
	return nativeValue[uintptr](p), nil
}

func (c *sessionConnection) verifyFiles(wal bool) error {
	main, err := c.filePointer(sqlite3.SQLITE_FCNTL_FILE_POINTER)
	if err != nil {
		return err
	}
	if err = verifySessionFile(c.bound, main, ""); err != nil {
		return err
	}
	if wal {
		journal, err := c.filePointer(sqlite3.SQLITE_FCNTL_JOURNAL_POINTER)
		if err != nil {
			return err
		}
		if journal != 0 && nativeValue[sqlite3.Tsqlite3_file](journal).FpMethods != 0 {
			if err = verifySessionFile(c.bound, journal, "-wal"); err != nil {
				return err
			}
		}
		return verifySessionSHM(c.bound, main)
	}
	return nil
}

func (c *sessionConnection) query(ctx context.Context, query string, args []string, visit func(map[string]any) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		tls := libc.NewTLS()
		sqlite3.Xsqlite3_interrupt(tls, c.db)
		tls.Close()
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	text, err := libc.CString(query)
	if err != nil {
		return err
	}
	defer libc.Xfree(c.tls, text)
	ptr := c.tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer c.tls.Free(int(unsafe.Sizeof(uintptr(0))))
	clear(libc.GoBytes(ptr, int(unsafe.Sizeof(uintptr(0)))))
	rc := sqlite3.Xsqlite3_prepare_v2(c.tls, c.db, text, -1, ptr, 0)
	stmt := nativeValue[uintptr](ptr)
	if stmt != 0 {
		defer sqlite3.Xsqlite3_finalize(c.tls, stmt)
	}
	if rc != sqlite3.SQLITE_OK {
		return c.err(rc)
	}
	for i, arg := range args {
		p, err := libc.CString(arg)
		if err != nil {
			return err
		}
		rc = sqlite3.Xsqlite3_bind_text(c.tls, stmt, int32(i+1), p, int32(len(arg)), ^uintptr(0))
		libc.Xfree(c.tls, p)
		if rc != sqlite3.SQLITE_OK {
			return c.err(rc)
		}
	}
	for {
		rc = sqlite3.Xsqlite3_step(c.tls, stmt)
		if err := ctx.Err(); err != nil {
			return err
		}
		switch rc {
		case sqlite3.SQLITE_DONE:
			return nil
		case sqlite3.SQLITE_ROW:
			if visit == nil {
				continue
			}
			row := map[string]any{}
			for i, n := int32(0), sqlite3.Xsqlite3_column_count(c.tls, stmt); i < n; i++ {
				name := libc.GoString(sqlite3.Xsqlite3_column_name(c.tls, stmt, i))
				var value any
				switch sqlite3.Xsqlite3_column_type(c.tls, stmt, i) {
				case sqlite3.SQLITE_INTEGER:
					value = sqlite3.Xsqlite3_column_int64(c.tls, stmt, i)
				case sqlite3.SQLITE_FLOAT:
					value = sqlite3.Xsqlite3_column_double(c.tls, stmt, i)
				case sqlite3.SQLITE_TEXT, sqlite3.SQLITE_BLOB:
					p := sqlite3.Xsqlite3_column_blob(c.tls, stmt, i)
					n := sqlite3.Xsqlite3_column_bytes(c.tls, stmt, i)
					if n > 0 {
						value = string(libc.GoBytes(p, int(n)))
					} else {
						value = ""
					}
				}
				row[name] = value
			}
			if err := visit(row); err != nil {
				return err
			}
		default:
			return c.err(rc)
		}
	}
}

// Native structs contain C addresses as uintptr values, never Go pointers.
func nativeValue[T any](p uintptr) (v T) {
	n := int(unsafe.Sizeof(v))
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&v)), n), libc.GoBytes(p, n))
	return v
}
