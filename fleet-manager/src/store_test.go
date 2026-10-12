package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memoryObject struct {
	raw     []byte
	version int
}
type memoryStore struct {
	mu         sync.Mutex
	objects    map[string]memoryObject
	versioning bool
	// Test knobs: a store that refuses writes, and one slow enough to expose a serial read.
	failPut    bool
	failGetIn  string
	delayGetIn string
	getDelay   time.Duration
	hashes     map[string]StoredHashes
	hashReads  int
}

func (s *memoryStore) versionOf(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key].version
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string]memoryObject{}, versioning: true, hashes: map[string]StoredHashes{}}
}
func (s *memoryStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if s.getDelay > 0 && (s.delayGetIn == "" || strings.Contains(key, s.delayGetIn)) {
		timer := time.NewTimer(s.getDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGetIn != "" && strings.Contains(key, s.failGetIn) {
		return nil, "", errorsNew("store is unavailable")
	}
	object, ok := s.objects[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	// The providers' read limit, so a test sees what a cloud store would answer.
	if len(object.raw) > stateObjectLimit {
		return nil, "", ErrTooLarge
	}
	return append([]byte(nil), object.raw...), strconv.Itoa(object.version), nil
}
func (s *memoryStore) Create(_ context.Context, key string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; ok {
		return ErrConflict
	}
	s.objects[key] = memoryObject{raw: append([]byte(nil), raw...), version: 1}
	return nil
}
func (s *memoryStore) Replace(_ context.Context, key, expected string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return ErrNotFound
	}
	if strconv.Itoa(object.version) != expected {
		return ErrConflict
	}
	object.raw, object.version = append([]byte(nil), raw...), object.version+1
	s.objects[key] = object
	return nil
}
func (s *memoryStore) Put(_ context.Context, key string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut {
		return errorsNew("store is unavailable")
	}
	object := s.objects[key]
	object.raw, object.version = append([]byte(nil), raw...), object.version+1
	s.objects[key] = object
	return nil
}
func (s *memoryStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ObjectInfo
	for key, object := range s.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, ObjectInfo{Key: key, Version: strconv.Itoa(object.version)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (s *memoryStore) ListPrefixes(_ context.Context, prefix, delimiter string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool)
	for key := range s.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if end := strings.Index(rest, delimiter); end >= 0 {
			seen[prefix+rest[:end+len(delimiter)]] = true
		}
	}
	var out []string
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
func (s *memoryStore) VersioningEnabled(context.Context) (bool, error) { return s.versioning, nil }

// setStoredHashes stands in for a shipper's direct presigned PUT, which this store never sees.
func (s *memoryStore) setStoredHashes(key string, hashes StoredHashes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashes[key] = hashes
}
func (s *memoryStore) StoredHashes(_ context.Context, key string) (StoredHashes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashReads++
	hashes, ok := s.hashes[key]
	if !ok {
		return StoredHashes{}, ErrNotFound
	}
	return hashes, nil
}
