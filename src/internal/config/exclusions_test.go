package config_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/bmatcuk/doublestar/v4"
)

func TestSourceAdditionsFollowAllReplacementLayers(t *testing.T) {
	const id = "claude-code-transcripts"
	compiled, _ := loadCatalog(t).Source(id)
	files := []string{"projects/public/session.jsonl", "projects/private/session.jsonl", "projects/local/session.jsonl", "projects/local-added/session.jsonl", "projects/remote/session.jsonl", "projects/demo/tool-results/image.png"}
	for _, tc := range []struct {
		name, local, remote string
		want                []string
		excluded            []string
	}{
		{"catalog", "", "exclude_add: ['projects/private/**']", append(slices.Clone(compiled.Exclude), "projects/private/**"),
			[]string{"projects/private/session.jsonl", "projects/demo/tool-results/image.png"}},
		{"local replacement", "exclude: ['projects/local/**']", "exclude_add: ['projects/private/**']", []string{"projects/local/**", "projects/private/**"},
			[]string{"projects/private/session.jsonl", "projects/local/session.jsonl"}},
		{"every layer adds", "exclude: ['projects/local/**'], exclude_add: ['projects/local-added/**']", "exclude: ['projects/remote/**'], exclude_add: ['projects/private/**', 'projects/local-added/**']", []string{"projects/remote/**", "projects/local-added/**", "projects/private/**"},
			[]string{"projects/private/session.jsonl", "projects/local-added/session.jsonl", "projects/remote/session.jsonl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var layers []config.LayeredDocument
			if tc.local != "" {
				layers = append(layers, config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "sources: [{id: "+id+", "+tc.local+"}]")})
			}
			layers = append(layers, served(t, "sources: [{id: "+id+", "+tc.remote+"}]"))
			eff, err := resolveLayers(t, layers...)
			if err != nil {
				t.Fatal(err)
			}
			got := sourceByID(t, eff, id).Exclude
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("exclusions %v, want %v", got, tc.want)
			}
			for _, file := range files {
				matched := slices.ContainsFunc(got, func(pattern string) bool {
					ok, err := doublestar.Match(pattern, file)
					if err != nil {
						t.Fatal(err)
					}
					return ok
				})
				if matched != slices.Contains(tc.excluded, file) {
					t.Errorf("%q excluded: %v", file, matched)
				}
			}
		})
	}
	again, _ := loadCatalog(t).Source(id)
	if !reflect.DeepEqual(compiled.Exclude, again.Exclude) {
		t.Fatal("resolution mutated the catalog")
	}
}

func TestAdditionsSurviveRemoteEnableOverLocalDisable(t *testing.T) {
	eff, err := resolveLayers(t,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "sources: [{id: claude-code-transcripts, enabled: false, exclude_add: ['local/**']}]")},
		served(t, "sources: [{id: claude-code-transcripts, enabled: true, exclude_add: ['remote/**']} ]"))
	if err != nil {
		t.Fatal(err)
	}
	source := sourceByID(t, eff, "claude-code-transcripts")
	if source.Enabled || !slices.Contains(source.Exclude, "local/**") || !slices.Contains(source.Exclude, "remote/**") {
		t.Fatalf("lost local refusal or additions: %+v", source)
	}
}

func TestAdditiveExclusionsRejectUnmatchableGlobsLocallyAndRemotely(t *testing.T) {
	for _, layer := range []config.Layer{config.LayerUser, config.LayerRemote} {
		for _, pattern := range []string{`projects\private\**`, `projects/private\**`, `projects\private/**`,
			"projects//private/**", "projects/private/", "./projects/private/**", "~/.claude/projects/private/**", "$CLAUDE_CONFIG_DIR/projects/**"} {
			_, err := resolveLayers(t, config.LayeredDocument{
				Layer: layer,
				Doc:   doc(t, "sources: [{id: claude-code-transcripts, exclude_add: ['"+pattern+"']}]"),
			})
			mustReject(t, err, "unmatchable additive exclusion "+pattern)
		}
	}
}
