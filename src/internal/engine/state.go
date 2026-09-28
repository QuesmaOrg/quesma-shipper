// The local fingerprint store is authoritative for upload progress: no backend tracks what was
// uploaded. One JSON document, replaced atomically under a process flock; retry is re-run, so a
// wiped or unloadable document costs a re-hash and a per-object probe, never a lost file.
package engine

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// StateSchema versions the document. A mismatch means downgrade or corruption: reject, never guess.
const StateSchema = 1

// The lock is a separate file so the document itself is only ever replaced, never held open.
const (
	FileName = "fingerprints.json"
	lockName = "fingerprints.lock"
)

// maxDocumentBytes bounds the document: roughly a hundred thousand entries at 400 bytes each.
const maxDocumentBytes = 64 << 20

var (
	// ErrLocked means another flush holds the store; the caller should try later rather than wait.
	ErrLocked = errors.New("state: store is locked by another flush")

	// ErrSchemaMismatch means the document was written by a different version.
	ErrSchemaMismatch = errors.New("state: document schema mismatch")
)

// Key identifies one fingerprint. ID is the candidate's logical identity when its source declares
// one, else its absolute native path, so every path-keyed entry stays valid.
type Key struct {
	SourceID string
	ID       string
}

// KeyOf is the state key a discovered candidate is recorded under.
func KeyOf(sourceID string, c sources.Candidate) Key {
	return Key{SourceID: sourceID, ID: cmp.Or(c.Identity, c.Path)}
}

// observedPath is where the file behind k was last seen.
func observedPath(k Key, fp Fingerprint) string { return cmp.Or(fp.NativePath, k.ID) }

// Fingerprint is what the store remembers about one file.
type Fingerprint struct {
	// NativePath is where the file was last observed. Only a logical-identity key needs it, as its
	// ID is not a path Prune could test; empty means the key ID is the path.
	NativePath string

	// Size and mtime are the cheap pre-filter; the content hash is the authority. SourceHash also
	// marks a completed ship: only the post-verified-PUT commit may write it, never a failure path.
	SourceSize  int64
	SourceMTime time.Time
	SourceHash  string

	// Derived entries only.
	Enricher   *EnricherRef
	OutputHash string

	// A parked entry waits for backoff. There is no max-retry: giving up is silent data loss.
	Parked       bool
	LastError    string
	BackoffUntil time.Time

	// Attempts counts consecutive failures on this file, turning retry-every-tick into a backoff.
	Attempts int
}

// EnricherRef identifies the enricher that produced a derived entry: the manifest's own shape,
// which is also this document's wire shape.
type EnricherRef = transforms.EnricherRef

// Document is the whole on-disk state, as read by Peek.
type Document struct {
	InstallID   string
	UpdatedAt   time.Time
	SourceSpecs map[string]string
	Entries     map[Key]Fingerprint
}

// ForeignTo reports whether another install wrote this document. An unstamped one belongs to
// whoever opens it, so an empty id on either side is never foreign.
func (d Document) ForeignTo(installID string) bool {
	return d.InstallID != "" && installID != "" && d.InstallID != installID
}

// Store is an open, locked fingerprint store.
type Store struct {
	dir       string
	installID string
	lock      *os.File
	specs     map[string]string
	entries   map[Key]Fingerprint

	corrupt bool
}

// Corrupt says the document could not be loaded and was discarded: the run continues from an empty
// store and the first flush replaces the file. Carried out so the discard is reported, not survived.
func (s *Store) Corrupt() bool { return s.corrupt }

// Open takes the flock, non-blocking, and loads the document: a busy store is refused, not queued.
func Open(stateDir, installID string) (*Store, error) {
	return open(stateDir, installID, maxDocumentBytes)
}

// pruneMaxDocumentBytes is what Prune and Reset may read: larger than the ordinary cap, still bounded.
const pruneMaxDocumentBytes = 512 << 20

func open(stateDir, installID string, maxBytes int64) (*Store, error) {
	if err := platform.EnsureDir(stateDir, 0o700); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(stateDir, lockName)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: open lock: %w", err)
	}
	if err := platform.LockFile(lock); err != nil {
		lock.Close()
		return nil, ErrLocked
	}
	s := &Store{dir: stateDir, installID: installID, lock: lock}

	// Any document that cannot be loaded is discarded, never fatal: the archive answers for what it
	// already holds, so an empty store costs a re-hash, not a re-upload.
	doc, err := load(stateDir, maxBytes)
	if err == nil && doc.ForeignTo(installID) {
		err = fmt.Errorf("state: %s belongs to install %s, this install is %s",
			filepath.Join(stateDir, FileName), doc.InstallID, installID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s could not be loaded (%v); continuing from an empty store\n", FileName, err)
		doc = Document{SourceSpecs: map[string]string{}, Entries: map[Key]Fingerprint{}}
		s.corrupt = true
	}
	s.specs, s.entries = doc.SourceSpecs, doc.Entries
	return s, nil
}

// Close releases the lock.
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	platform.UnlockFile(s.lock)
	err := s.lock.Close()
	s.lock = nil
	return err
}

// editStore is the shell both operator overrides share: it reads past maxDocumentBytes, since a
// past-the-ceiling document must not lock out the override, and writes only what edit changed.
// A discarded document is always written back: the override is the operator's chance to replace it.
func editStore(stateDir, installID string, dryRun bool, edit func(*Store) int) (int, error) {
	s, err := open(stateDir, installID, pruneMaxDocumentBytes)
	if err != nil {
		return 0, err
	}
	defer s.Close()

	changed := edit(s)
	if dryRun || (changed == 0 && !s.corrupt) {
		return changed, nil
	}
	return changed, s.flush()
}

// Prune removes entries whose file is gone. It tests existence on disk, so a file on an unmounted
// volume reads as gone, which is why it stays a command.
func Prune(stateDir, installID string, dryRun bool) (removed, kept int, err error) {
	removed, err = editStore(stateDir, installID, dryRun, func(s *Store) int {
		gone := 0
		for k, fp := range s.entries {
			if _, statErr := os.Lstat(observedPath(k, fp)); statErr == nil {
				kept++
				continue
			}
			gone++
			delete(s.entries, k)
		}
		return gone
	})
	return removed, kept, err
}

// Reset forgets every fingerprint, so the next sync re-hashes the whole history and re-probes the
// archive; unchanged bytes come back already_present, so a reset never forces a re-seal.
// The document is replaced with an empty one rather than deleted, so the install id survives.
func Reset(stateDir, installID string, dryRun bool) (removed int, err error) {
	return editStore(stateDir, installID, dryRun, func(s *Store) int {
		removed := len(s.entries)
		s.entries = map[Key]Fingerprint{}
		return removed
	})
}

// Peek reads the document without the lock, so status and doctor never contend with a flush.
func Peek(stateDir string) (Document, error) {
	return load(stateDir, maxDocumentBytes)
}

// Get returns a fingerprint.
func (s *Store) Get(k Key) (Fingerprint, bool) {
	fp, ok := s.entries[k]
	return fp, ok
}

// Len reports how many entries the store holds.
func (s *Store) Len() int { return len(s.entries) }

// CommitAll records several fingerprints in one document replacement. The only durable step in the
// loop, and it happens last: a crash before the replace re-runs those files onto their existing
// keys. Safe under retry-is-re-run. Never make this a database.
func (s *Store) CommitAll(updates map[Key]Fingerprint) error {
	for k, fp := range updates {
		s.entries[k] = fp
	}
	return s.flush()
}

// SpecFor reports the spec generation this source's entries were recorded under.
func (s *Store) SpecFor(sourceID string) (string, bool) {
	stored, known := s.specs[sourceID]
	return stored, known
}

// EnsureSpec records which spec generation this source's entries belong to, dropping them all when
// it differs. Per source and never the global config_version, which would invalidate every
// fingerprint on every machine. Entries with no recorded generation adopt it without dropping.
func (s *Store) EnsureSpec(sourceID, specFP string) (dropped int, err error) {
	stored, known := s.specs[sourceID]
	if known && stored == specFP {
		return 0, nil
	}
	if known {
		for k := range s.entries {
			if k.SourceID == sourceID {
				delete(s.entries, k)
				dropped++
			}
		}
	}
	if s.specs == nil {
		s.specs = map[string]string{}
	}
	s.specs[sourceID] = specFP
	return dropped, s.flush()
}

// DropVanished forgets this source's entries whose file discovery no longer sees, keyed by key ID.
// Only a source that collected AND returned candidates proves absence: err on kept-too-long.
func (s *Store) DropVanished(sourceID string, live map[string]bool) (int, error) {
	dropped := 0
	for k := range s.entries {
		if k.SourceID != sourceID {
			continue
		}
		if live[k.ID] {
			continue
		}
		delete(s.entries, k)
		dropped++
	}
	if dropped == 0 {
		return 0, nil
	}
	return dropped, s.flush()
}

func (s *Store) flush() error {
	body, err := encode(s.installID, time.Now().UTC().Truncate(time.Second), s.specs, s.entries)
	if err != nil {
		return err
	}
	return platform.WriteAtomic(filepath.Join(s.dir, FileName), body, 0o600)
}

// --- wire format ------------------------------------------------------------

type wireDoc struct {
	StateSchema int               `json:"state_schema"`
	InstallID   string            `json:"install_id,omitempty"`
	UpdatedAt   string            `json:"updated_at,omitempty"`
	SourceSpecs map[string]string `json:"source_specs,omitempty"`

	// A source_hash corrupted in place still parses and reads as a completed ship, which is silent
	// permanent loss: "a lost document only costs a re-ship" holds for forgetting a ship, never for
	// falsely remembering one. Absent on documents written before this field existed.
	Checksum string `json:"checksum,omitempty"`

	Entries []wireEntry `json:"entries"`
}

// Hashed with the checksum field cleared, so both sides compute over the same bytes. Marshal, never
// MarshalIndent: formatting must be free to change without invalidating every store in the fleet.
func checksumOf(doc wireDoc) (string, error) {
	doc.Checksum = ""
	body, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("state: checksum: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

type wireEntry struct {
	SourceID     string       `json:"source_id"`
	NativePath   string       `json:"native_path"`
	Identity     string       `json:"identity,omitempty"`
	SourceSize   int64        `json:"source_size,omitempty"`
	SourceMTime  string       `json:"source_mtime,omitempty"`
	Attempts     int          `json:"attempts,omitempty"`
	SourceHash   string       `json:"source_hash,omitempty"`
	Enricher     *EnricherRef `json:"enricher,omitempty"`
	OutputHash   string       `json:"output_hash,omitempty"`
	Parked       bool         `json:"parked,omitempty"`
	LastError    string       `json:"last_error,omitempty"`
	BackoffUntil string       `json:"backoff_until,omitempty"`
}

// encode serializes deterministically: entries sorted by key, so equal state gives equal bytes.
func encode(installID string, updatedAt time.Time, specs map[string]string, entries map[Key]Fingerprint) ([]byte, error) {
	// json.Marshal writes map keys sorted, so source_specs is deterministic too.
	doc := wireDoc{StateSchema: StateSchema, InstallID: installID, SourceSpecs: specs, Entries: []wireEntry{}}
	if !updatedAt.IsZero() {
		doc.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	}

	keys := slices.SortedFunc(maps.Keys(entries), func(a, b Key) int {
		return cmp.Or(strings.Compare(a.SourceID, b.SourceID), strings.Compare(a.ID, b.ID))
	})

	for _, k := range keys {
		fp := entries[k]
		e := wireEntry{
			SourceID:   k.SourceID,
			NativePath: observedPath(k, fp),
			SourceSize: fp.SourceSize,
			SourceHash: fp.SourceHash,
			OutputHash: fp.OutputHash,
			Attempts:   fp.Attempts,
			Parked:     fp.Parked,
			LastError:  fp.LastError,
			Enricher:   fp.Enricher,
		}
		// Only for a logical identity, so path-keyed entries keep their old bytes. Not additive for a rollback: an older
		// binary fails the checksum on it and discards the store once (re-hash, uploads already_present, Codex keys revert).
		if e.NativePath != k.ID {
			e.Identity = k.ID
		}
		if !fp.SourceMTime.IsZero() {
			// RFC3339Nano, not RFC3339: the pre-filter compares this against the file's mtime for
			// EXACT equality, so whole seconds here re-read and re-hash every file on every tick.
			// Rounding both sides instead would skip a same-second change. Do not lower the precision.
			e.SourceMTime = fp.SourceMTime.UTC().Format(time.RFC3339Nano)
		}
		if !fp.BackoffUntil.IsZero() {
			e.BackoffUntil = fp.BackoffUntil.UTC().Format(time.RFC3339)
		}
		doc.Entries = append(doc.Entries, e)
	}

	sum, err := checksumOf(doc)
	if err != nil {
		return nil, err
	}
	doc.Checksum = sum

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("state: encode: %w", err)
	}
	body = append(body, '\n')

	// Validate on the way out: a state file that fails its own schema is a bug to catch here.
	if err := validate(body); err != nil {
		return nil, err
	}
	return body, nil
}

func load(stateDir string, maxBytes int64) (Document, error) {
	path := filepath.Join(stateDir, FileName)
	raw, _, err := platform.ReadWhole(path, maxBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First run. An empty store is not an error: everything is simply unshipped.
			return Document{Entries: map[Key]Fingerprint{}}, nil
		}
		return Document{}, fmt.Errorf("state: read %s: %w", path, err)
	}

	// No JSON Schema pass here: encode already proved the SHAPE, and re-parsing every run only
	// re-proves it. The checksum below is a different question and cheap enough to ask every time:
	// it covers the VALUES, which the schema never did. Unknown fields drop on the next rewrite and
	// a missing field reads zero, failing toward a re-ship onto the same key. A negative attempts
	// count must be refused: it panics as the backoff's shift.
	var doc wireDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Document{}, fmt.Errorf("state: parse %s: %w", path, err)
	}
	// The only schema guard on the load path: rejecting costs one re-upload, guessing costs more.
	if doc.StateSchema != StateSchema {
		return Document{}, fmt.Errorf("%w: document says %d, this client speaks %d",
			ErrSchemaMismatch, doc.StateSchema, StateSchema)
	}
	// Absent means written before the field existed; it earns one on the next rewrite. A wrong one
	// fails the whole load, because a partly-trusted store is the failure this catches.
	if doc.Checksum != "" {
		want, err := checksumOf(doc)
		if err != nil {
			return Document{}, err
		}
		if want != doc.Checksum {
			return Document{}, fmt.Errorf("state: %s failed its checksum", path)
		}
	}

	out := Document{
		InstallID:   doc.InstallID,
		SourceSpecs: doc.SourceSpecs,
		Entries:     make(map[Key]Fingerprint, len(doc.Entries)),
	}
	if out.SourceSpecs == nil {
		out.SourceSpecs = map[string]string{}
	}
	if doc.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, doc.UpdatedAt); err == nil {
			out.UpdatedAt = t
		}
	}
	for _, e := range doc.Entries {
		if e.Attempts < 0 {
			return Document{}, fmt.Errorf("state: entry %s %s: negative attempts %d",
				e.SourceID, e.NativePath, e.Attempts)
		}
		fp := Fingerprint{
			SourceSize: e.SourceSize,
			SourceHash: e.SourceHash,
			OutputHash: e.OutputHash,
			Attempts:   e.Attempts,
			Parked:     e.Parked,
			LastError:  e.LastError,
			Enricher:   e.Enricher,
		}
		if e.SourceMTime != "" {
			if t, err := time.Parse(time.RFC3339, e.SourceMTime); err == nil {
				fp.SourceMTime = t
			}
		}
		if e.BackoffUntil != "" {
			if t, err := time.Parse(time.RFC3339, e.BackoffUntil); err == nil {
				fp.BackoffUntil = t
			}
		}
		// A path-keyed entry leaves NativePath empty: its key ID is the path.
		if e.Identity != "" {
			fp.NativePath = e.NativePath
		}
		out.Entries[Key{SourceID: e.SourceID, ID: cmp.Or(e.Identity, e.NativePath)}] = fp
	}
	return out, nil
}

func validate(raw []byte) error {
	err := formats.ValidateRaw(formats.FingerprintState, raw)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, formats.ErrNotJSON):
		return fmt.Errorf("state: document is %w", err)
	}
	return fmt.Errorf("state: document does not satisfy its schema: %w", err)
}
