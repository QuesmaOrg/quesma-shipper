package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

func (p *Accounts) collectClaude(ctx context.Context, req Request) ([]accountObservation, bool) {
	var out []accountObservation
	env := req.Env
	if env.Lookup == nil {
		env.Lookup = func(string) (string, bool) { return "", false }
	}
	home := req.Source.Root
	config := filepath.Join(env.Home, ".claude.json")
	service := "Claude Code-credentials"
	override, _ := env.Lookup("CLAUDE_CONFIG_DIR")
	if home != filepath.Join(env.Home, ".claude") || override != "" && filepath.Clean(override) == home {
		config = filepath.Join(home, ".claude.json")
		keychainHome := home
		if override != "" && filepath.Clean(override) == home {
			keychainHome = override
		}
		sum := sha256.Sum256([]byte(keychainHome))
		service = fmt.Sprintf("Claude Code-credentials-%x", sum[:4])
	}
	doc, err := accountJSON(config, 64<<20)
	if err != nil || len(doc["oauthAccount"]) > 0 {
		out = append(out, localAccount(req, "claude.local.oauthAccount", doc["oauthAccount"], err))
	}
	var raw []byte
	if runtime.GOOS == "darwin" && err == nil && bytes.HasPrefix(bytes.TrimSpace(doc["oauthAccount"]), []byte("{")) {
		read := p.keychain
		if read == nil {
			read = readAccountKeychain
		}
		raw, _ = read(ctx, service)
	}
	if len(raw) == 0 {
		raw, _, _ = platform.ReadWhole(filepath.Join(home, ".credentials.json"), accountResponseLimit)
	}
	var creds struct {
		OAuth struct {
			Token  string   `json:"accessToken"`
			Scopes []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	credentialErr := json.Unmarshal(raw, &creds)
	token := creds.OAuth.Token
	scoped := false
	for _, scope := range creds.OAuth.Scopes {
		if scope == "user:profile" {
			scoped = true
		}
	}
	if credentialErr != nil || !scoped {
		token = ""
	}
	out = append(out, p.fetch(ctx, req, "claude.oauth.profile", "GET", "https://api.anthropic.com/api/oauth/profile", token, ""))
	out = append(out, p.fetch(ctx, req, "claude.oauth.usage", "GET", "https://api.anthropic.com/api/oauth/usage", token, ""))
	return out, true
}
