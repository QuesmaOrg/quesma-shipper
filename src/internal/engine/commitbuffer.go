package engine

// commitBuffer batches fingerprint commits: every Commit re-encodes, validates and fsyncs every
// entry, so committing per file costs O(files x entries). An unflushed commit re-ships that file
// onto the same key, which bounds a crash to the flush limit rather than to one file.
type commitBuffer struct {
	store   *Store
	pending map[Key]Fingerprint

	// staleSpecs masks sources whose stored spec differs, set by PreviewSpec on dry-run only:
	// preview must persist nothing, so it hides entries instead of dropping them.
	staleSpecs map[string]bool

	// limit bounds both the crash window and the memory held.
	limit int

	flushes int // for tests: this type exists to control how often it writes
}

const defaultCommitBatch = 200

func newCommitBuffer(store *Store, limit int) *commitBuffer {
	if limit <= 0 {
		limit = defaultCommitBatch
	}
	return &commitBuffer{
		store:   store,
		pending: make(map[Key]Fingerprint),
		limit:   limit,
	}
}

// Get reads through the buffer: a fingerprint committed earlier in this run but not yet flushed
// is still the truth about what shipped.
func (b *commitBuffer) Get(k Key) (Fingerprint, bool) {
	if b.staleSpecs[k.SourceID] {
		return Fingerprint{}, false
	}
	if fp, ok := b.pending[k]; ok {
		return fp, true
	}
	return b.store.Get(k)
}

func (b *commitBuffer) Commit(k Key, fp Fingerprint) error {
	b.pending[k] = fp
	if len(b.pending) >= b.limit {
		return b.Flush()
	}
	return nil
}

// Flush makes everything buffered durable in one document replacement.
func (b *commitBuffer) Flush() error {
	if len(b.pending) == 0 {
		return nil
	}
	if err := b.store.CommitAll(b.pending); err != nil {
		return err
	}
	clear(b.pending)
	b.flushes++
	return nil
}

// The rest of the store's surface passes straight through; none of it has anything to batch.

func (b *commitBuffer) EnsureSpec(sourceID, specFingerprint string) (int, error) {
	if err := b.Flush(); err != nil {
		return 0, err
	}
	return b.store.EnsureSpec(sourceID, specFingerprint)
}

// PreviewSpec is EnsureSpec's dry-run counterpart: it persists nothing and drops nothing, but
// masks a source whose stored spec differs, so preview reports the same would-ship as a sync.
func (b *commitBuffer) PreviewSpec(sourceID, specFingerprint string) {
	if stored, known := b.store.SpecFor(sourceID); known && stored != specFingerprint {
		if b.staleSpecs == nil {
			b.staleSpecs = map[string]bool{}
		}
		b.staleSpecs[sourceID] = true
	}
}

// DropVanished forwards the GC, flushing first: a pending commit for a file the walk did not see
// would otherwise be written back immediately after being forgotten.
func (b *commitBuffer) DropVanished(sourceID string, live map[string]bool) (int, error) {
	if err := b.Flush(); err != nil {
		return 0, err
	}
	return b.store.DropVanished(sourceID, live)
}
