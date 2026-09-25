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
	VersioningEnabled(context.Context) (bool, error)
	// SourceHash reads the stored object's source-hash metadata without its body; ErrNotFound
	// when the key is absent, empty when the object carries none.
	SourceHash(context.Context, string) (string, error)
}

type ObjectInfo struct {
	Key     string
	Version string
}

func objectKeys(objects []ObjectInfo) []string {
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = object.Key
	}
	return keys
}

var (
	ErrNotFound = errors.New("object not found")
	ErrConflict = errors.New("conditional write conflict")
)
