package sources

// One sign-in and billing record per agent, in the shape of formats.AgentAuth. Each field is copied
// by name from what the agent stores, so a token next to it on disk cannot reach the record.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

const agentAuthSchema = "agent-auth/v1"

type agentAuth struct {
	Schema            string       `json:"schema"`
	Agent             string       `json:"agent"`
	Billing           string       `json:"billing"`
	AuthMethod        string       `json:"auth_method"`
	Provider          string       `json:"provider,omitempty"`
	Plan              string       `json:"plan,omitempty"`
	SubscriptionUntil string       `json:"subscription_until,omitempty"`
	Account           *authAccount `json:"account,omitempty"`
	Evidence          []string     `json:"evidence"`
}

type authAccount struct {
	Email   string `json:"email,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	OrgID   string `json:"org_id,omitempty"`
	OrgName string `json:"org_name,omitempty"`
}

func newAgentAuth(agent string) agentAuth {
	return agentAuth{Schema: agentAuthSchema, Agent: agent, Billing: "unknown", AuthMethod: "unknown", Evidence: []string{}}
}

func (a *agentAuth) set(billing, method, provider, evidence string) {
	a.Billing, a.AuthMethod, a.Provider = billing, method, provider
	a.Evidence = append(a.Evidence, evidence)
}

func (a *agentAuth) setAccount(v authAccount) {
	if v != (authAccount{}) {
		a.Account = &v
	}
}

func authObservation(req Request, auth agentAuth) accountObservation {
	body, err := json.Marshal(auth)
	return localAccount(req, auth.Agent+".auth", body, err)
}

type claudeAuthInput struct {
	OAuthAccount json.RawMessage
	HasAPIKey    bool
	Credentials  []byte
	Settings     map[string]json.RawMessage
}

// claudeAuth follows Claude Code's own order: a cloud provider, then a configured key, then the login.
func claudeAuth(in claudeAuthInput) agentAuth {
	auth := newAgentAuth("claude")
	var env map[string]string
	_ = json.Unmarshal(in.Settings["env"], &env)
	for _, cloud := range []struct{ flag, provider string }{
		{"CLAUDE_CODE_USE_BEDROCK", "bedrock"}, {"CLAUDE_CODE_USE_VERTEX", "vertex"}, {"CLAUDE_CODE_USE_FOUNDRY", "foundry"},
	} {
		if envTrue(env[cloud.flag]) {
			auth.set("cloud", "cloud_credentials", cloud.provider, "settings.json:env."+cloud.flag)
			return auth
		}
	}
	var account struct {
		AccountUUID      string `json:"accountUuid"`
		Email            string `json:"emailAddress"`
		OrganizationUUID string `json:"organizationUuid"`
		OrganizationName string `json:"organizationName"`
		OrganizationType string `json:"organizationType"`
		BillingType      string `json:"billingType"`
	}
	_ = json.Unmarshal(in.OAuthAccount, &account)
	auth.setAccount(authAccount{Email: account.Email, UserID: account.AccountUUID, OrgID: account.OrganizationUUID, OrgName: account.OrganizationName})
	switch {
	case jsonPresent(in.Settings["apiKeyHelper"]):
		auth.set("api", "api_key", "anthropic", "settings.json:apiKeyHelper")
		return auth
	case env["ANTHROPIC_API_KEY"] != "":
		auth.set("api", "api_key", "anthropic", "settings.json:env.ANTHROPIC_API_KEY")
		return auth
	case in.HasAPIKey:
		auth.set("api", "api_key", "anthropic", ".claude.json:primaryApiKey")
		return auth
	}
	var creds struct {
		OAuth struct {
			AccessToken      string `json:"accessToken"`
			SubscriptionType string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	_ = json.Unmarshal(in.Credentials, &creds)
	if plan := creds.OAuth.SubscriptionType; plan != "" {
		auth.set("subscription", "oauth", "anthropic", "credentials:claudeAiOauth.subscriptionType")
		auth.Plan = plan
		return auth
	}
	if strings.Contains(account.BillingType, "subscription") {
		auth.set("subscription", "oauth", "anthropic", ".claude.json:oauthAccount.billingType")
		auth.Plan = strings.TrimPrefix(account.OrganizationType, "claude_")
		return auth
	}
	switch {
	case account.AccountUUID != "":
		auth.AuthMethod, auth.Provider = "oauth", "anthropic"
		auth.Evidence = append(auth.Evidence, ".claude.json:oauthAccount")
	case creds.OAuth.AccessToken != "":
		auth.AuthMethod, auth.Provider = "oauth", "anthropic"
		auth.Evidence = append(auth.Evidence, "credentials:claudeAiOauth")
	}
	return auth
}

func jsonPresent(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func envTrue(v string) bool { return v == "1" || strings.EqualFold(v, "true") }

// Codex providers that serve models from the same machine.
var codexLocalProviders = map[string]bool{"oss": true, "ollama": true, "lmstudio": true}

// codexAuth weighs config.toml before auth.json: a non-OpenAI provider is used whatever login is stored.
func codexAuth(doc map[string]json.RawMessage, claims []byte, config []byte) agentAuth {
	auth := newAgentAuth("codex")
	var identity struct {
		Email string `json:"email"`
		Auth  struct {
			Plan       string `json:"chatgpt_plan_type"`
			Until      string `json:"chatgpt_subscription_active_until"`
			UserID     string `json:"chatgpt_user_id"`
			AccountID  string `json:"chatgpt_account_id"`
			Workspaces []struct {
				ID      string `json:"id"`
				Title   string `json:"title"`
				Default bool   `json:"is_default"`
			} `json:"organizations"`
		} `json:"https://api.openai.com/auth"`
	}
	_ = json.Unmarshal(claims, &identity)
	account := authAccount{Email: identity.Email, UserID: identity.Auth.UserID, OrgID: identity.Auth.AccountID}
	for _, w := range identity.Auth.Workspaces {
		if w.Default || len(identity.Auth.Workspaces) == 1 {
			account.OrgName = w.Title
		}
	}
	auth.setAccount(account)
	if provider := tomlTopLevelString(config, "model_provider"); provider != "" && provider != "openai" {
		billing, method := "api", "api_key"
		if codexLocalProviders[provider] {
			billing, method = "local", "none"
		}
		auth.set(billing, method, provider, "config.toml:model_provider")
		return auth
	}
	var mode, key string
	_ = json.Unmarshal(doc["auth_mode"], &mode)
	_ = json.Unmarshal(doc["OPENAI_API_KEY"], &key)
	switch {
	case mode == "chatgpt" || mode == "chatgptAuthTokens":
		auth.set("subscription", "oauth", "openai", "auth.json:auth_mode")
		auth.Plan = identity.Auth.Plan
		if until, err := time.Parse(time.RFC3339, identity.Auth.Until); err == nil {
			auth.SubscriptionUntil = until.UTC().Format(time.RFC3339)
		}
	case mode == "apikey" || mode == "" && key != "":
		auth.set("api", "api_key", "openai", "auth.json:auth_mode")
	}
	return auth
}

// tomlTopLevelString reads one string key before the first table, enough for Codex's top-level settings.
func tomlTopLevelString(raw []byte, key string) string {
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if strings.HasPrefix(line, "[") {
			return ""
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(value)
		if i := strings.Index(value, "#"); i >= 0 && !strings.HasPrefix(value, `"`) {
			value = strings.TrimSpace(value[:i])
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
			if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
				return value[1 : end+1]
			}
		}
		return ""
	}
	return ""
}

// cursorAuth reads the plan Cursor caches at sign-in; every Cursor plan, free included, is billed by Cursor.
func cursorAuth(values map[string]json.RawMessage, signedIn bool) agentAuth {
	auth := newAgentAuth("cursor")
	var plan, email string
	var team struct {
		ID   json.RawMessage `json:"teamId"`
		Name string          `json:"name"`
	}
	_ = json.Unmarshal(values["cursorAuth/stripeMembershipType"], &plan)
	_ = json.Unmarshal(values["cursorAuth/cachedEmail"], &email)
	_ = json.Unmarshal(values["cursorAuth/cachedTeam"], &team)
	auth.setAccount(authAccount{Email: email, OrgID: strings.Trim(string(team.ID), `"`), OrgName: team.Name})
	switch {
	case plan != "":
		auth.set("subscription", "oauth", "cursor", "state.vscdb:cursorAuth/stripeMembershipType")
		auth.Plan = plan
	case signedIn:
		auth.set("subscription", "oauth", "cursor", "state.vscdb:cursorAuth/accessToken")
	}
	return auth
}
