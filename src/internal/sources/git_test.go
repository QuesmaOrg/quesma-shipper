package sources

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestNormaliseRemoteKeepsHostAndPathOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://github.com/Acme/api.git", "github.com/Acme/api"},
		{"https://jane:ghp_secret@GitHub.com/Acme/api/", "github.com/Acme/api"},
		{"git@github.com:Acme/api.git", "github.com/Acme/api"},
		{"ssh://git@github.com:2222/Acme/api.git", "github.com/Acme/api"},
		{"git://github.com/Acme/api", "github.com/Acme/api"},
		{"github.com:Acme/api", "github.com/Acme/api"},
		{"https://gitlab.com/group/sub/api.git", "gitlab.com/group/sub/api"},
		{"git@github-work:Acme/api.git", "github-work/Acme/api"},
		{"/srv/git/api.git", ""},
		{"../api", ""},
		{`C:\repos\api`, ""},
		{"file:///srv/git/api.git", ""},
		{"https://github.com/", ""},
		{"", ""},
	} {
		if got := normaliseRemote(tc.in); got != tc.want {
			t.Errorf("normaliseRemote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Only remote URLs leave the config: a header carrying a token in the same file never does.
func TestGitRemotesReadsOnlyRemoteURLs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config"), `[core]
	bare = false
[remote "origin"]
	url = https://jane:ghp_secret@github.com/Acme/api.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[http]
	extraHeader = "AUTHORIZATION: bearer sekrit"
	url = https://not-a-remote.example/x
[remote "upstream"]
	URL = "git@github.com:Up/api.git" ; the canonical one
[remote.legacy]
	url = git@gitlab.com:Acme/api.git # old
[remote "local"]
	url = /srv/git/api.git
`)
	got := gitRemotes(dir)
	want := []gitRemote{
		{"legacy", "gitlab.com/Acme/api"},
		{"origin", "github.com/Acme/api"},
		{"upstream", "github.com/Up/api"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The remote shown for a directory: origin first, then upstream, then whichever there is, with a
// count of the other addresses; none outside git.
func TestAttributionRemoteNamesTheRepository(t *testing.T) {
	home := t.TempDir()
	cfg := func(rel, body string) string {
		dir := gitRepo(t, home, rel)
		writeFile(t, filepath.Join(dir, ".git", "config"), body)
		return dir
	}
	fork := cfg("fork", "[remote \"upstream\"]\n\turl = git@github.com:Up/api.git\n[remote \"origin\"]\n\turl = git@github.com:Me/api.git\n")
	up := cfg("up", "[remote \"upstream\"]\n\turl = git@github.com:Up/api.git\n[remote \"mirror\"]\n\turl = https://gitlab.com/Up/api\n")
	same := cfg("same", "[remote \"origin\"]\n\turl = git@github.com:Me/api.git\n[remote \"push\"]\n\turl = https://github.com/Me/api\n")
	none := gitRepo(t, home, "none")
	plain := repoDir(t, home, "plain", false)

	f := newAttribution(testProbe(), testGitRead(), home)
	for dir, want := range map[string]string{
		fork:                     "github.com/Me/api +1",
		filepath.Join(up, "sub"): "github.com/Up/api +1",
		same:                     "github.com/Me/api",
		none:                     "",
		plain:                    "",
	} {
		if got := f.Remote(dir); got != want {
			t.Errorf("Remote(%s) = %q, want %q", dir, got, want)
		}
	}
}
