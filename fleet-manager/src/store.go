package main

import (
	"context"
	"errors"
)

// ObjectStore is the only durable-state primitive used by the service. Version is opaque to
// callers; providers map it to ETags or generations and must enforce each conditional operation.
type ObjectStore interface {
	Get(context.Context, string) ([]byte, string, error)
	Create(context.Context, string, []byte) error
	Replace(context.Context, string, string, []byte) error
	// Put writes unconditionally. It is for records that can be regenerated at will, so providers
	// may mark them for lifecycle expiry rather than keeping every version forever.
	Put(context.Context, string, []byte) error
	List(context.Context, string) ([]ObjectInfo, error)
	ListPrefixes(context.Context, string, string) ([]string, error)
	VersioningEnabled(context.Context) (bool, error)
	// StoredHashes reads the stored object's hash metadata without its body; ErrNotFound when
	// the key is absent, an empty field when the object carries none.
	StoredHashes(context.Context, string) (StoredHashes, error)
}

// StoredHashes: Source covers an object's raw bytes, Shipped the scrubbed bytes that were sealed.
type StoredHashes struct{ Source, Shipped string }

type ObjectInfo struct {
	Key     string
	Version string
}

var (
	ErrNotFound = errors.New("object not found")
	ErrConflict = errors.New("conditional write conflict")
)
