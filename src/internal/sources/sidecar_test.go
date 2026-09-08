package sources_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// A remote carrying a live credential must normalise to host/org/repo with the token absent from every output.
func TestRemoteNormalisationStripsUserinfo(t *testing.T) {
	cases := []struct {
		name, raw, wantHostPath, wantProject string
		wantErr                              bool
	}{
		{"https with token", "https://user:ghp_abcdefghijklmnopqrstuvwxyz0123456789@github.com/org/repo.git", "github.com/org/repo", "repo", false},
		{"https with user only", "https://jane@github.com/org/repo", "github.com/org/repo", "repo", false},
		{"https plain", "https://github.com/org/repo.git", "github.com/org/repo", "repo", false},
		{"ssh scp form", "git@github.com:org/repo.git", "github.com/org/repo", "repo", false},
		{"ssh url form", "ssh://git@github.com/org/repo.git", "github.com/org/repo", "repo", false},
		{"non-default port collapses", "ssh://git@github.com:2222/org/repo.git", "github.com/org/repo", "repo", false},
		{"nested group", "https://gitlab.com/group/sub/repo.git", "gitlab.com/group/sub/repo", "repo", false},
		{"uppercase host", "https://GitHub.com/Org/Repo.git", "github.com/Org/Repo", "Repo", false},
		{"local path rejected", "/Users/jane/src/thing", "", "", true},
		{"file scheme rejected", "file:///Users/jane/src/thing", "", "", true},
		{"empty rejected", "", "", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hostPath, project, err := sources.NormaliseRemote(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected a refusal, got %q", hostPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hostPath != c.wantHostPath {
				t.Errorf("host/path: got %q want %q", hostPath, c.wantHostPath)
			}
			if project != c.wantProject {
				t.Errorf("project: got %q want %q", project, c.wantProject)
			}
			// The token must be gone from every part of the output, not merely from the host.
			for _, secret := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "user:", "jane@"} {
				if strings.Contains(hostPath+project, secret) {
					t.Errorf("output leaks %q: %s %s", secret, hostPath, project)
				}
			}
		})
	}
}

// The full path: a transcript names a cwd inside a checkout whose remote carries a token.
func TestSidecarEmitsMappingWithoutTheToken(t *testing.T) {
	home := t.TempDir()
	checkout := filepath.Join(home, "work", "api")
	write(t, filepath.Join(checkout, ".git", "config"), `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = https://jane:ghp_abcdefghijklmnopqrstuvwxyz0123456789@github.com/acme/api.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[credential]
	helper = osxkeychain
`)

	agentRoot := filepath.Join(home, ".claude")
	write(t, filepath.Join(agentRoot, "projects", "-Users-jane-work-api", "s1.jsonl"),
		`{"type":"user","uuid":"u1","cwd":`+strconv.Quote(checkout)+`,"message":{"content":[]}}`+"\n")

	transcripts := source(agentRoot, []string{"projects/**/*.jsonl"})
	sidecar := sidecarSource()
	stateDir := t.TempDir()

	reg := sources.NewRegistry()
	p, err := reg.For("sidecar")
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Discover(sources.Request{
		Source:   sidecar,
		All:      []config.ResolvedSource{transcripts, sidecar},
		Deny:     sources.New(home),
		StateDir: stateDir,
		Username: "jane",
	})
	if err != nil {
		t.Fatal(err)
	}

	if d.Health != sources.Collected {
		t.Fatalf("health %q reason %q", d.Health, d.Reason)
	}
	// One inventory object, not one per file.
	if len(d.Candidates) != 1 {
		t.Fatalf("expected exactly one inventory candidate, got %d", len(d.Candidates))
	}

	body, err := os.ReadFile(d.Candidates[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("the token reached the inventory:\n%s", body)
	}
	if strings.Contains(string(body), "jane:") {
		t.Errorf("userinfo survived:\n%s", body)
	}

	var rec sources.ProjectRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(body))), &rec); err != nil {
		t.Fatalf("inventory is not JSONL: %v\n%s", err, body)
	}
	if rec.Remote != "github.com/acme/api" {
		t.Errorf("remote %q, want github.com/acme/api", rec.Remote)
	}
	if rec.Project != "api" {
		t.Errorf("project %q", rec.Project)
	}
	// Placeholdered, and the join still holds: a shipped manifest's native_path carries the same rewritten username.
	if rec.ProjectDir != "-Users-__USER__-work-api" {
		t.Errorf("project_dir %q — it is the only join key in trajectory paths, and the "+
			"trajectory side is placeholdered", rec.ProjectDir)
	}
	if strings.Contains(rec.CWD, "jane") {
		t.Errorf("cwd should carry the placeholder: %q", rec.CWD)
	}
}

// project = none is a legal outcome, and the record says WHY rather than being silently absent.
func TestSidecarRecordsGiveUpReasons(t *testing.T) {
	home := t.TempDir()
	noRepo := filepath.Join(home, "scratch")
	if err := os.MkdirAll(noRepo, 0o700); err != nil {
		t.Fatal(err)
	}

	agentRoot := filepath.Join(home, ".claude")
	write(t, filepath.Join(agentRoot, "projects", "-Users-jane-scratch", "s1.jsonl"),
		`{"type":"user","uuid":"u1","cwd":`+strconv.Quote(noRepo)+`,"message":{"content":[]}}`+"\n")

	rec := runSidecar(t, home, agentRoot)
	if rec.Remote != "" {
		t.Errorf("expected no remote, got %q", rec.Remote)
	}
	if rec.GaveUp == "" {
		t.Error("a gap must be explained rather than merely empty")
	}
	if !strings.Contains(rec.GaveUp, ".git") {
		t.Errorf("give-up reason should name what was looked for: %q", rec.GaveUp)
	}
}

// A worktree's .git is a FILE holding a gitdir: pointer. The dir it names usually holds
// no config, only a commondir pointing at the common git dir that does; without one the
// pointer already names the dir with the config.
func TestSidecarFollowsWorktreeGitdirPointer(t *testing.T) {
	for _, withCommondir := range []bool{true, false} {
		home := t.TempDir()
		gitDir := filepath.Join(home, "repos", "api", ".git", "worktrees", "wt")
		configDir := gitDir
		if withCommondir {
			configDir = filepath.Join(home, "repos", "api", ".git")
			write(t, filepath.Join(gitDir, "commondir"), "../..\n")
		}
		write(t, filepath.Join(configDir, "config"), "[remote \"origin\"]\n\turl = git@github.com:acme/api.git\n")

		worktree := filepath.Join(home, "work", "wt")
		if err := os.MkdirAll(worktree, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(worktree, ".git"), "gitdir: "+gitDir+"\n")

		agentRoot := filepath.Join(home, ".claude")
		write(t, filepath.Join(agentRoot, "projects", "-Users-jane-work-wt", "s1.jsonl"),
			`{"type":"user","uuid":"u1","cwd":`+strconv.Quote(worktree)+`,"message":{"content":[]}}`+"\n")

		rec := runSidecar(t, home, agentRoot)
		if rec.Remote != "github.com/acme/api" {
			t.Errorf("commondir=%v: remote %q, gave up %q", withCommondir, rec.Remote, rec.GaveUp)
		}
	}
}

// The probe walks up from cwd, since an agent's cwd is usually below the checkout root.
func TestSidecarWalksUpToTheRepositoryRoot(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "work", "api")
	write(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/api.git\n")
	deep := filepath.Join(repo, "src", "internal", "db")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}

	agentRoot := filepath.Join(home, ".claude")
	write(t, filepath.Join(agentRoot, "projects", "-Users-jane-work-api", "s1.jsonl"),
		`{"type":"user","uuid":"u1","cwd":`+strconv.Quote(deep)+`,"message":{"content":[]}}`+"\n")

	rec := runSidecar(t, home, agentRoot)
	if rec.Remote != "github.com/acme/api" {
		t.Errorf("walk-up failed: remote %q, gave up %q", rec.Remote, rec.GaveUp)
	}
}

// Codex nests cwd under payload, and the field list is config rather than code so that difference costs nothing.
func TestSidecarReadsANestedCWDField(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "work", "api")
	write(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = https://github.com/acme/api.git\n")

	agentRoot := filepath.Join(home, ".claude")
	write(t, filepath.Join(agentRoot, "projects", "-Users-jane-work-api", "r.jsonl"),
		`{"timestamp":"t","type":"session_meta","payload":{"cwd":`+strconv.Quote(repo)+`}}`+"\n")

	rec := runSidecar(t, home, agentRoot)
	if rec.Remote != "github.com/acme/api" {
		t.Errorf("nested cwd field not read: remote %q gave up %q", rec.Remote, rec.GaveUp)
	}
}

// A cwd that no longer exists is a chosen give-up case: the trajectory outlives the checkout.
func TestSidecarHandlesAVanishedCWD(t *testing.T) {
	home := t.TempDir()
	agentRoot := filepath.Join(home, ".claude")
	write(t, filepath.Join(agentRoot, "projects", "-Users-jane-gone", "s1.jsonl"),
		`{"type":"user","uuid":"u1","cwd":`+strconv.Quote(filepath.Join(home, "deleted", "long", "ago"))+`,"message":{"content":[]}}`+"\n")

	rec := runSidecar(t, home, agentRoot)
	if rec.GaveUp == "" {
		t.Error("a vanished cwd should be recorded as a give-up, not an error")
	}
}

// --- ordering ----------------------------------------------------------------

// Candidates are oldest first: for a store that deletes itself, the file closest to deletion cannot be collected later.
func TestOldestFileIsFirstInLine(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "projects", "p", "old.jsonl")
	fresh := filepath.Join(root, "projects", "p", "fresh.jsonl")
	write(t, old, `{"a":1}`+"\n")
	write(t, fresh, `{"a":2}`+"\n")

	longAgo := mustParse(t, "2020-01-01T00:00:00Z")
	if err := os.Chtimes(old, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	d := discover(t, source(root, []string{"projects/**/*.jsonl"}), nil)
	// It must be first in line.
	if !strings.HasSuffix(d.Candidates[0].RelPath, "old.jsonl") {
		t.Errorf("the file closest to deletion must be collected first, got %s", d.Candidates[0].RelPath)
	}
}

// --- helpers ----------------------------------------------------------------

func sidecarSource() config.ResolvedSource {
	return config.ResolvedSource{
		Source: sources.Source{
			ID:            "project-map",
			Family:        "project-map",
			Gather:        "sidecar",
			ArtifactClass: "context",
			Emit:          "git_project_map",
			CWDProbe: &sources.CWDProbe{
				From:      []string{"claude-code-transcripts"},
				Fields:    []string{"cwd", "payload.cwd"},
				ScanBytes: 65536,
			},
			GitRead: &sources.GitRead{
				WalkUp:           true,
				FollowGitdirFile: true,
				Take:             []string{"remote.*.url"},
			},
		},
		Enabled: true,
	}
}

func runSidecar(t *testing.T, home, agentRoot string) sources.ProjectRecord {
	t.Helper()

	transcripts := source(agentRoot, []string{"projects/**/*.jsonl"})
	sc := sidecarSource()

	reg := sources.NewRegistry()
	p, err := reg.For("sidecar")
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Discover(sources.Request{
		Source:   sc,
		All:      []config.ResolvedSource{transcripts, sc},
		Deny:     sources.New(home),
		StateDir: t.TempDir(),
		Username: "jane",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 1 {
		t.Fatalf("expected one inventory, got %d (health %q reason %q)",
			len(d.Candidates), d.Health, d.Reason)
	}
	body, err := os.ReadFile(d.Candidates[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(body))
	if line == "" {
		t.Fatal("inventory is empty")
	}
	var rec sources.ProjectRecord
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &rec); err != nil {
		t.Fatalf("inventory is not JSONL: %v\n%s", err, body)
	}
	return rec
}

// Regression: a path with no projects/<encoded-cwd> segment must produce no record; a bogus join key is worse than a missing one.
func TestSidecarEmitsNoRecordWithoutAProjectDirSegment(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "work", "api")
	write(t, filepath.Join(repo, ".git", "config"),
		"[remote \"origin\"]\n\turl = https://github.com/acme/api.git\n")

	codexRoot := filepath.Join(home, ".codex")
	write(t, filepath.Join(codexRoot, "sessions", "2026", "07", "30", "rollout-x.jsonl"),
		`{"timestamp":"t","type":"session_meta","payload":{"cwd":`+strconv.Quote(repo)+`}}`+"\n")

	rollouts := source(codexRoot, []string{"sessions/**/*.jsonl"})
	rollouts.ID = "codex-rollouts"
	sc := sidecarSource()
	sc.CWDProbe.From = []string{"codex-rollouts"}

	reg := sources.NewRegistry()
	p, err := reg.For("sidecar")
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Discover(sources.Request{
		Source:   sc,
		All:      []config.ResolvedSource{rollouts, sc},
		Deny:     sources.New(home),
		StateDir: t.TempDir(),
		Username: "jane",
	})
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(d.Candidates[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"project_dir":"sessions"`) {
		t.Errorf("a date-sharded path produced a bogus join key:\n%s", body)
	}
	if strings.TrimSpace(string(body)) != "" {
		t.Errorf("expected no records for a source with no projects/ segment:\n%s", body)
	}
}
