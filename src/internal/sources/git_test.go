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
