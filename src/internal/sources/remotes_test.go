package sources

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

func TestNormaliseRemoteKeepsHostAndPathApart(t *testing.T) {
	for _, tc := range []struct{ in, host, path string }{
		{"https://github.com/Acme/api.git", "github.com", "Acme/api"},
		{"https://jane:ghp_secret@GitHub.com/Acme/api/", "github.com", "Acme/api"},
		{"git@github.com:Acme/api.git", "github.com", "Acme/api"},
		{"ssh://git@github.com:2222/Acme/api.git", "github.com", "Acme/api"},
		{"git://github.com/Acme/api", "github.com", "Acme/api"},
		{"https://gitlab.com/group/sub/api.git", "gitlab.com", "group/sub/api"},
		{"jane@devbox:/home/jane/api.git", "devbox", "/home/jane/api"},
		{"devbox:projects/api", "devbox", "projects/api"},
		{"git@github-work:Acme/api.git", "github-work", "Acme/api"},
		{"/srv/git/api.git", "", ""},
		{"../api", "", ""},
		{`C:\repos\api`, "", ""},
		{"file:///srv/git/api.git", "", ""},
		{"https://github.com/", "", ""},
		{"", "", ""},
		{"ext::sshpass -p hunter2 ssh git@host %S repo", "", ""},
		{"fd::17/foo", "", ""},
		{"jane:ghp_secret@github.com:Acme/api.git", "", ""},
	} {
		if host, path := normaliseRemote(tc.in); host != tc.host || path != tc.path {
			t.Errorf("normaliseRemote(%q) = %q, %q, want %q, %q", tc.in, host, path, tc.host, tc.path)
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
[Remote "mixed"]
	url = git@github.com:Mixed/api.git
[remote "local"]
	url = /srv/git/api.git
`)
	want := []formats.Remote{
		{Name: "legacy", Host: "gitlab.com", Path: "Acme/api"},
		{Name: "mixed", Host: "github.com", Path: "Mixed/api"},
		{Name: "origin", Host: "github.com", Path: "Acme/api"},
		{Name: "upstream", Host: "github.com", Path: "Up/api"},
	}
	if got := gitRemotes(dir); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
