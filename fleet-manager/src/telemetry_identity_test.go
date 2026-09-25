package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type identityRaceStore struct {
	ObjectStore
	arrivals chan struct{}
	release  chan struct{}
}

func (s *identityRaceStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	raw, version, err := s.ObjectStore.Get(ctx, key)
	if key == telemetryIdentityKey && errors.Is(err, ErrNotFound) {
		s.arrivals <- struct{}{}
		<-s.release
	}
	return raw, version, err
}

func TestTelemetryIdentityPersistsAcrossConcurrentReplicaStarts(t *testing.T) {
	store := newMemoryStore()
	const replicas = 16
	racing := &identityRaceStore{ObjectStore: store, arrivals: make(chan struct{}, replicas), release: make(chan struct{})}
	manager, _ := NewManager(racing)
	type result struct {
		record telemetryRegistration
		err    error
	}
	results := make(chan result, replicas)
	for range replicas {
		go func() {
			identity, key, err := manager.loadTelemetryIdentity(context.Background())
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{record: registration(identity.FleetManagerID, key)}
		}()
	}
	for range replicas {
		<-racing.arrivals
	}
	close(racing.release)
	var want telemetryRegistration
	for index := range replicas {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if index == 0 {
			want = got.record
		}
		if got.record != want {
			t.Fatal("replicas provisioned different identities")
		}
	}
	if store.versionOf(telemetryIdentityKey) != 1 {
		t.Fatal("identity was overwritten during provisioning")
	}
	restarted, _ := NewManager(store)
	identity, key, err := restarted.loadTelemetryIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if registration(identity.FleetManagerID, key) != want {
		t.Fatal("restart rotated the identity")
	}
}

func TestTelemetryIdentityNeverReplacesCorruptOrUnreadableState(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"seed":"invalid"}`), []byte(`{"seed":"secret","fleet_manager_id":"invalid","unknown":"field"}`), []byte(`not JSON`)} {
		store := newMemoryStore()
		if err := store.Create(context.Background(), telemetryIdentityKey, raw); err != nil {
			t.Fatal(err)
		}
		manager, _ := NewManager(store)
		if _, _, err := manager.loadTelemetryIdentity(context.Background()); err == nil {
			t.Fatal("invalid persisted identity accepted")
		}
		after, _, _ := store.Get(context.Background(), telemetryIdentityKey)
		if !bytes.Equal(after, raw) || store.versionOf(telemetryIdentityKey) != 1 {
			t.Fatal("corrupt identity silently replaced")
		}
	}
	store := newMemoryStore()
	store.failGetIn = telemetryIdentityKey
	manager, _ := NewManager(store)
	if _, _, err := manager.loadTelemetryIdentity(context.Background()); err == nil {
		t.Fatal("storage failure ignored")
	}
	if store.versionOf(telemetryIdentityKey) != 0 {
		t.Fatal("generated a replacement when the store was unreadable")
	}
}

type failedIdentityCreateStore struct{ ObjectStore }

func (s failedIdentityCreateStore) Create(context.Context, string, []byte) error {
	return errors.New("storage write failed")
}

func TestTelemetryIdentityIsNotUsedUntilPersisted(t *testing.T) {
	manager, _ := NewManager(failedIdentityCreateStore{newMemoryStore()})
	_, key, err := manager.loadTelemetryIdentity(context.Background())
	if err == nil || key != nil {
		t.Fatal("using an identity that was not persisted")
	}
}
