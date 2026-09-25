package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
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
	hashes     map[string]string
	hashReads  int
}

func (s *memoryStore) versionOf(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key].version
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string]memoryObject{}, versioning: true, hashes: map[string]string{}}
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
func (s *memoryStore) VersioningEnabled(context.Context) (bool, error) { return s.versioning, nil }

// setSourceHash stands in for a shipper's direct presigned PUT, which this store never sees.
func (s *memoryStore) setSourceHash(key, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashes[key] = hash
}
func (s *memoryStore) SourceHash(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashReads++
	hash, ok := s.hashes[key]
	if !ok {
		return "", ErrNotFound
	}
	return hash, nil
}

func TestObjectStoreConformance(t *testing.T) {
	ctx, store := context.Background(), newMemoryStore()
	if _, _, err := store.Get(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("absent get: %v", err)
	}
	if err := store.Create(ctx, "prefix/a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "prefix/a", []byte("two")); err != ErrConflict {
		t.Fatalf("duplicate create: %v", err)
	}
	_, version, _ := store.Get(ctx, "prefix/a")
	if err := store.Replace(ctx, "prefix/a", "stale", []byte("two")); err != ErrConflict {
		t.Fatalf("stale replace: %v", err)
	}
	if err := store.Replace(ctx, "prefix/a", version, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "prefix/a", []byte("three")); err != nil {
		t.Fatalf("put over existing: %v", err)
	}
	if err := store.Put(ctx, "prefix/new", []byte("four")); err != nil {
		t.Fatalf("put of absent: %v", err)
	}
	if raw, _, _ := store.Get(ctx, "prefix/a"); string(raw) != "three" {
		t.Fatalf("put did not overwrite: %q", raw)
	}
	_ = store.Create(ctx, "other/b", []byte("x"))
	listed, err := store.List(ctx, "prefix/")
	if err != nil || len(listed) != 2 || listed[0].Key != "prefix/a" {
		t.Fatalf("list: %#v, %v", listed, err)
	}
}

func TestObjectStoreAllowsOneConcurrentWriter(t *testing.T) {
	ctx, store := context.Background(), newMemoryStore()
	_ = store.Create(ctx, "object", []byte("zero"))
	_, version, _ := store.Get(ctx, "object")
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results <- store.Replace(ctx, "object", version, []byte(fmt.Sprint(i))) }(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if err != ErrConflict {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successful writers = %d, want 1", success)
	}
}

func TestStrictRecordDecode(t *testing.T) {
	var record GrantRecord
	if err := strictDecode([]byte(`{"schema":1,"id":"x","unknown":true}`), &record); err == nil {
		t.Fatal("unknown record field was accepted")
	}
}
