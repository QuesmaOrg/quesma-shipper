package sources

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testHome = "/home/u"

// MatchTree licenses a walk to skip a directory, so only a "<root>/**" rule may answer yes.
func TestMatchTreeOnlyAnswersForWholeTrees(t *testing.T) {
	d := New(testHome)

	for _, dir := range []string{
		testHome + "/.ssh",
		testHome + "/.ssh/keys",
		testHome + "/Library/Keychains",
	} {
		if denied, _ := d.MatchTree(dir); !denied {
			t.Errorf("%s is a denied tree", dir)
		}
	}
	for _, dir := range []string{
		// Denied as a path by a basename rule, but its contents are not.
		testHome + "/proj/.env",
		testHome + "/proj/release.key",
		// Denied as a path exactly, same reasoning.
		testHome + "/.netrc",
		testHome + "/proj",
	} {
		if denied, pat := d.MatchTree(dir); denied {
			t.Errorf("%s was pruned as a tree by %q, which denies only the path itself", dir, pat)
		}
	}
}

// realTempDir resolves its own symlinks, so a test comparing given and resolved forms is not measuring /var -> /private/var.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMatchPairChecksBothForms(t *testing.T) {
	home := realTempDir(t)
	mkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(filepath.Join(home, ".ssh"))
	mkdir(filepath.Join(home, "work"))
	secret := filepath.Join(home, ".ssh", "id_secret")
	plain := filepath.Join(home, "work", "notes.jsonl")
	for _, p := range []string{secret, plain} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A benign name that resolves into the denied tree.
	intoDenied := filepath.Join(home, "work", "notes-link.jsonl")
	if err := os.Symlink(secret, intoDenied); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// And the other direction: a denied location pointing at a harmless file.
	outOfDenied := filepath.Join(home, ".ssh", "link.jsonl")
	if err := os.Symlink(plain, outOfDenied); err != nil {
		t.Fatal(err)
	}

	d := New(home)

	if denied, pat := d.MatchPair(intoDenied, secret); !denied {
		t.Error("a path resolving into the denied tree must be denied")
	} else if !strings.HasSuffix(pat, "/.ssh/**") {
		t.Errorf("reported %q, expected the ssh subtree pattern", pat)
	}
	if denied, _ := d.MatchPair(intoDenied, ""); denied {
		t.Error("without the resolved form only the literal path is checked, and it is clean")
	}
	if denied, _ := d.MatchPair(outOfDenied, plain); !denied {
		t.Error("a denied location is denied whatever it points at")
	}
	if denied, _ := d.MatchPair(plain, plain); denied {
		t.Error("an ordinary file must not be denied")
	}

	// Match resolves for itself and must reach the same verdicts.
	for _, p := range []string{intoDenied, outOfDenied, secret} {
		if denied, _ := d.Match(p); !denied {
			t.Errorf("Match(%s) should be denied", p)
		}
	}
	if denied, _ := d.Match(plain); denied {
		t.Errorf("Match(%s) should be allowed", plain)
	}
}

// Each row states the pattern the path must be reported against, or "" for one that stays collectable; "~" expands to testHome.
func TestMatchReportsTheFirstPatternInListOrder(t *testing.T) {
	type row struct{ path, want string }
	corpus := []row{
		// Denied roots, and the root itself.
		{"~/.ssh", "~/.ssh/**"},
		{"~/.ssh/config", "~/.ssh/**"},
		{"~/.ssh/nested/deeper/key", "~/.ssh/**"},
		{"~/.aws/credentials", "~/.aws/**"},
		{"~/.gnupg/pubring.kbx", "~/.gnupg/**"},
		{"~/.kube/config", "~/.kube/**"},
		{"~/.azure/msal_token_cache.json", "~/.azure/**"},
		{"~/.config/gh/hosts.yml", "~/.config/gh/**"},
		{"~/.config/gcloud", "~/.config/gcloud/**"},
		{"~/Library/Keychains/login.keychain-db", "~/Library/Keychains/**"},
		// Inside a denied tree AND matching an earlier pattern: the earlier one is reported, both directions.
		{"~/.ssh/id_rsa", "~/.ssh/**"},
		{"~/Library/Keychains/x.pem", "**/*.pem"},
		{"~/Library/Keychains/login.key", "**/*.key"},
		// Near misses on the roots.
		{"~/.sshfoo/config", ""},
		{"~/.ssh_backup", ""},
		{"~/.config/ghost/notes.jsonl", ""},
		// Exact files, and things next to them.
		{"~/.netrc", "~/.netrc"},
		{"~/.netrcx", ""},
		{"~/x/.netrc", ""},
		{"~/.npmrc", "~/.npmrc"},
		{"~/.pypirc", "~/.pypirc"},
		{"~/.git-credentials", "~/.git-credentials"},
		{"~/.claude.json", "~/.claude.json"},
		{"~/.claude.json.bak", ""},
		{"~/.claude/.credentials.json", "~/.claude/.credentials.json"},
		{"~/.claude/projects/p/a.jsonl", ""},
		{"~/.docker/config.json", "~/.docker/config.json"},
		{"~/.docker/daemon.json", ""},
		{"~/.codex/auth.json", "~/.codex/auth.json"},
		{"~/.config/opencode/auth.json", "~/.config/opencode/auth.json"},
		{"~/.local/share/opencode/auth.json", "~/.local/share/opencode/auth.json"},
		// Basenames, anywhere, and their near misses.
		{"~/proj/.env", "**/.env"},
		{"~/proj/.envy", ""},
		{"~/proj/.env.local", "**/.env.*"},
		{"~/proj/.env.", "**/.env.*"},
		{"~/proj/env", ""},
		{"~/proj/id_rsa", "**/id_rsa"},
		{"~/proj/id_rsa.pub", ""},
		{"~/proj/id_ed25519", "**/id_ed25519"},
		{"~/proj/deep/nest/.env", "**/.env"},
		{"/srv/shared/.env", "**/.env"},
		{"/.env", "**/.env"},
		// Extension rules.
		{"~/proj/server.pem", "**/*.pem"},
		{"~/proj/server.pem.old", ""},
		{"~/proj/.pem", "**/*.pem"},
		{"~/proj/bundle.p12", "**/*.p12"},
		{"~/proj/private.key", "**/*.key"},
		{"~/proj/keyfile", ""},
		// Ordinary files that must stay collectable.
		{"~/.claude/projects/p/session.jsonl", ""},
		{"/var/log/app.log", ""},
		{"/", ""},
	}

	if runtime.GOOS == "windows" {
		corpus = append(corpus, row{"~/.SSH/id_rsa", "~/.ssh/**"}, row{"~/proj/.ENV", "**/.env"},
			row{"~/proj/ID_RSA", "**/id_rsa"}, row{"~/proj/PRIVATE.KEY", "**/*.key"})
	} else {
		corpus = append(corpus, row{"~/.SSH/id_rsa", "**/id_rsa"}, row{"~/proj/.ENV", ""},
			row{"~/proj/ID_RSA", ""}, row{"~/proj/PRIVATE.KEY", ""})
	}
	if os.Getenv("APPDATA") != "" {
		corpus = append(corpus, row{"$APPDATA/gcloud/credentials.db", "$APPDATA/gcloud/**"},
			row{"$APPDATA/GitHub CLI/hosts.yml", "$APPDATA/GitHub CLI/**"})
	}

	exp := func(s string) string {
		if s == "" {
			return ""
		}
		return normalize(os.ExpandEnv(strings.Replace(s, "~", testHome, 1)))
	}
	d := New(testHome)
	reported := map[string]bool{}
	for _, row := range corpus {
		path, want := exp(row.path), exp(row.want)
		reported[want] = true
		ok, pat := d.matchCandidate(path)
		if ok != (want != "") || pat != want {
			t.Errorf("%s: reported (%v, %q), want (%v, %q)", path, ok, pat, want != "", want)
		}
	}
	// A new compiled pattern needs a path that reports against it, or the corpus stops covering the list.
	for _, pat := range d.Patterns() {
		if !reported[pat] {
			t.Errorf("no corpus path is reported against %q: add one", pat)
		}
	}
}
