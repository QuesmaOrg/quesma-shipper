package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func assertConfigAuthor(t *testing.T, manager *Manager, want configAuthor) FleetConfig {
	t.Helper()
	cfg, _, author, err := manager.LoadConfigWithAuthor(context.Background())
	if err != nil || author != want {
		t.Fatalf("configuration author = %q, %v; want %q", author, err, want)
	}
	return cfg
}

func TestConfigAuthorshipHTTP(t *testing.T) {
	server, store, partner := partnerServer(t)
	read := func(want configAuthor) string {
		t.Helper()
		w := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/config", "", partner))
		if w.Code != http.StatusOK || w.Header().Get("Config-Updated-By") != string(want) {
			t.Fatalf("GET config = %d, author %q: %s", w.Code, w.Header().Get("Config-Updated-By"), w.Body.String())
		}
		var legacy FleetConfig
		if err := strictDecode(w.Body.Bytes(), &legacy); err != nil {
			t.Fatalf("legacy strict response reader: %v", err)
		}
		return w.Header().Get("ETag")
	}
	read(configAuthorAdministrator)
	for _, tc := range []struct {
		path, credential, body string
		author                 configAuthor
	}{
		{"collection", partner, `{"collection":{"mode":{"schedule":"7m"}}}`, configAuthorPartner},
		{"collection", testAdminCredential, `{"collection":{"mode":{"schedule":"8m"}}}`, configAuthorAdministrator},
		{"config", testAdminCredential, configJSON(t, twoRecipients(t)), configAuthorAdministrator},
	} {
		before := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/config", "", testAdminCredential))
		put := adminRequest(http.MethodPut, "/v1/admin/orgs/acme/"+tc.path, tc.body, tc.credential)
		put.Header.Set("If-Match", before.Header().Get("ETag"))
		if w := serveAdmin(t, server, put); w.Code != http.StatusNoContent {
			t.Fatalf("PUT %s = %d: %s", tc.path, w.Code, w.Body.String())
		}
		read(tc.author)
	}
	raw, _, _ := store.Get(context.Background(), configKey("acme"))
	var legacy FleetConfig
	if err := strictDecode(raw, &legacy); err != nil || bytes.Contains(raw, []byte("updated_by")) {
		t.Fatalf("stored config changed its schema: %s (%v)", raw, err)
	}
	version := configVersion(t, store, "acme")
	for _, field := range []string{`"author":"administrator"`, `"updated_by":"administrator"`} {
		put := adminRequest(http.MethodPut, "/v1/admin/orgs/acme/collection", `{"collection":{},`+field+`}`, partner)
		put.Header.Set("If-Match", encodeETag(version))
		if w := serveAdmin(t, server, put); w.Code != http.StatusBadRequest {
			t.Fatalf("spoofed author = %d: %s", w.Code, w.Body.String())
		}
	}
	if configVersion(t, store, "acme") != version {
		t.Fatal("spoofed author changed configuration")
	}
}

type authorshipFaultStore struct {
	ObjectStore
	failAuthorship bool
	failConfig     bool
	beforeReplace  func()
}

func (s *authorshipFaultStore) Create(ctx context.Context, key string, raw []byte) error {
	if s.failAuthorship && strings.Contains(key, "/config-authorship/") {
		return errors.New("authorship write failed")
	}
	if s.failConfig && strings.HasSuffix(key, "/config.json") {
		return errors.New("configuration write failed")
	}
	return s.ObjectStore.Create(ctx, key, raw)
}

func (s *authorshipFaultStore) Replace(ctx context.Context, key, version string, raw []byte) error {
	if s.failConfig && strings.HasSuffix(key, "/config.json") {
		return errors.New("configuration write failed")
	}
	if s.beforeReplace != nil && strings.HasSuffix(key, "/config.json") {
		s.beforeReplace()
	}
	return s.ObjectStore.Replace(ctx, key, version, raw)
}

func TestConfigAuthorshipFailureAndRetry(t *testing.T) {
	for _, operation := range []string{"create", "replace"} {
		for _, failure := range []string{"authorship", "configuration"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				manager, store := testManager(t)
				ctx := context.Background()
				cfg := FleetConfig{AgeRecipients: twoRecipients(t), DisplayName: "Before"}
				version := ""
				if operation == "replace" {
					if err := manager.Init(ctx, configAuthorAdministrator, cfg); err != nil {
						t.Fatal(err)
					}
					version = configVersion(t, store, "acme")
				}
				fault := &authorshipFaultStore{ObjectStore: store, failAuthorship: failure == "authorship", failConfig: failure == "configuration"}
				manager.store = fault
				cfg.DisplayName = "After"
				write := func() error {
					if operation == "create" {
						return manager.Init(ctx, configAuthorAdministrator, cfg)
					}
					return manager.ApplyConfig(ctx, configAuthorPartner, cfg, version)
				}
				if err := write(); err == nil {
					t.Fatal("write succeeded through a failed store")
				}
				if operation == "create" {
					if _, _, err := manager.LoadConfig(ctx); !errors.Is(err, ErrNotFound) {
						t.Fatalf("failed create left a configuration: %v", err)
					}
				} else if cfg := assertConfigAuthor(t, manager, configAuthorAdministrator); cfg.DisplayName != "Before" {
					t.Fatal("failed change became active")
				}
				fault.failAuthorship, fault.failConfig = false, false
				if err := write(); err != nil {
					t.Fatalf("retry after failed write: %v", err)
				}
				want := configAuthorAdministrator
				if operation == "replace" {
					want = configAuthorPartner
				}
				if cfg := assertConfigAuthor(t, manager, want); cfg.DisplayName != "After" {
					t.Fatal("retry did not commit configuration")
				}
			})
		}
	}
}

func TestConfigAuthorshipConcurrentReplicas(t *testing.T) {
	manager, store := testManager(t)
	ctx := context.Background()
	if err := manager.Init(ctx, configAuthorAdministrator, FleetConfig{AgeRecipients: twoRecipients(t)}); err != nil {
		t.Fatal(err)
	}
	cfg, version, _ := manager.LoadConfig(ctx)
	prepared, resume := make(chan struct{}), make(chan struct{})
	slow := *manager
	slow.store = &authorshipFaultStore{ObjectStore: store, beforeReplace: func() { close(prepared); <-resume }}
	result := make(chan error, 1)
	go func() {
		loser := cfg
		loser.DisplayName = "Losing partner edit"
		result <- slow.ApplyConfig(ctx, configAuthorPartner, loser, version)
	}()
	select {
	case <-prepared:
	case err := <-result:
		t.Fatalf("writer failed before the race: %v", err)
	}
	cfg.DisplayName = "Winning administrator edit"
	err := manager.ApplyConfig(ctx, configAuthorAdministrator, cfg, version)
	close(resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrConflict) {
		t.Fatalf("losing writer = %v", err)
	}
	if cfg := assertConfigAuthor(t, manager, configAuthorAdministrator); cfg.DisplayName != "Winning administrator edit" {
		t.Fatalf("wrong configuration won: %+v", cfg)
	}
	records, _ := store.List(ctx, controlPrefix("acme")+"config-authorship/")
	if len(records) != 3 {
		t.Fatalf("expected initial, committed and unused records; got %d", len(records))
	}
}

func TestConfigAuthorshipLegacyAndReadFailures(t *testing.T) {
	server, store := testAdminServer(t)
	manager := server.manager.mustOrg(t, "acme")
	ctx := context.Background()
	cfg, version, _ := manager.LoadConfig(ctx)
	cfg.UpdatedAt = cfg.UpdatedAt.Add(time.Minute)
	cfg.DisplayName = "Legacy writer"
	if err := replaceRecord(ctx, store, configKey("acme"), version, cfg); err != nil {
		t.Fatal(err)
	}
	assertConfigAuthor(t, manager, configAuthorUnknown)
	w := serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/config", "", testAdminCredential))
	if w.Code != http.StatusOK || w.Header().Get("Config-Updated-By") != "unknown" {
		t.Fatalf("legacy GET = %d, %s", w.Code, w.Body.String())
	}
	store.failGetIn = "/config-authorship/"
	w = serveAdmin(t, server, adminRequest(http.MethodGet, "/v1/admin/orgs/acme/config", "", testAdminCredential))
	if w.Code != http.StatusInternalServerError || w.Header().Get("Config-Updated-By") != "" {
		t.Fatalf("failed authorship read must not look unknown: %d, %s", w.Code, w.Body.String())
	}
	if _, _, err := manager.LoadConfig(ctx); err != nil {
		t.Fatalf("shipper config load should not depend on audit reads: %v", err)
	}
	store.failGetIn = ""
	raw, _, _ := store.Get(ctx, configKey("acme"))
	for _, record := range []configAuthorship{
		{Schema: 2, Author: configAuthorPartner, UpdatedAt: cfg.UpdatedAt},
		{Schema: 1, Author: configAuthorUnknown, UpdatedAt: cfg.UpdatedAt},
		{Schema: 1, Author: configAuthorPartner},
	} {
		body, _ := json.Marshal(record)
		if err := store.Put(ctx, configAuthorshipKey("acme", raw), body); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := manager.LoadConfigWithAuthor(ctx); err == nil {
			t.Fatalf("accepted invalid authorship: %+v", record)
		}
	}
}

func TestConfigAuthorshipIdenticalBytesCannotChangeAuthor(t *testing.T) {
	manager, store := testManager(t)
	ctx := context.Background()
	if err := manager.Init(ctx, configAuthorAdministrator, FleetConfig{AgeRecipients: twoRecipients(t)}); err != nil {
		t.Fatal(err)
	}
	cfg, version, _ := manager.LoadConfig(ctx)
	if err := manager.ApplyConfig(ctx, configAuthorPartner, cfg, version); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("same bytes with a different author must fail explicitly: %v", err)
	}
	if configVersion(t, store, "acme") != version {
		t.Fatal("conflicting attribution changed configuration")
	}
	assertConfigAuthor(t, manager, configAuthorAdministrator)
	if err := manager.ApplyConfig(ctx, configAuthorUnknown, cfg, version); err == nil {
		t.Fatal("write without explicit author succeeded")
	}
}
