// Package accountprobe reports account and plan from each agent's auth store, files the glob
// pipeline denies because they also hold tokens. Output is allowlist-only: a fixed struct is
// marshalled, so tokens and unknown keys are unrepresentable rather than filtered.
//
// An account object is a timeline fact, never an attribution of a session. Readers bind a
// session to the account state whose interval contains its payload_mtime; joining against the
// latest known account misattributes everything shipped after a switch.
package accountprobe

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Account is the only shape a probe can emit, and field order is the marshal order, which is
// what makes the output deterministic.
type Account struct {
	Agent string `json:"agent"`

	// For codex this comes from the id_token's claims; the token never reaches the output.
	Email string `json:"email,omitempty"`

	// Plan is claude organizationType, codex chatgpt_plan_type, cursor stripeMembershipType.
	Plan string `json:"plan,omitempty"`

	// RateLimitTier refines the plan (claude's max 5x/20x split lives here, not in the plan).
	RateLimitTier string `json:"rate_limit_tier,omitempty"`

	AuthMode  string `json:"auth_mode,omitempty"`
	BillingTy string `json:"billing_type,omitempty"`
	OrgName   string `json:"org_name,omitempty"`
	OrgRole   string `json:"org_role,omitempty"`
	SeatTier  string `json:"seat_tier,omitempty"`
	Team      string `json:"team,omitempty"`

	// ActiveUntil is the subscription's recorded end, where the agent stores one (codex).
	ActiveUntil string `json:"active_until,omitempty"`
}

// ~/.claude.json also holds per-project state and grows to megabytes, so the read is bounded.
const maxStoreBytes = 64 << 20

// derivedFrom is the hash of the store bytes, not of the staged raw units the probes never
// consult, so provenance names the input that actually determined the output.
func emit(res *transforms.EnrichResult, storePath, derivedFrom string, acct Account) {
	out, err := json.Marshal(acct)
	if err != nil {
		res.Errors++
		res.Notes = append(res.Notes, "account: marshal failed: "+err.Error())
		return
	}
	res.Objects = append(res.Objects, transforms.Derived{
		NativePath:  storePath + ".account.enriched.json",
		Payload:     out,
		DerivedFrom: []string{derivedFrom},
		OutputHash:  transforms.Hash(out),
		Status:      transforms.StatusOK,
	})
}

func readStore(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxStoreBytes {
		return nil, fmt.Errorf("store is %d bytes, over the %d cap", st.Size(), maxStoreBytes)
	}
	return os.ReadFile(path)
}

// skippedAll marks the whole flush as nothing-to-derive with one reason.
func skippedAll(res transforms.EnrichResult, in transforms.Input, note string) transforms.EnrichResult {
	res.Skipped = len(in.Units)
	if len(in.Units) > 0 && note != "" {
		res.Notes = append(res.Notes, note)
	}
	return res
}

func failed(res transforms.EnrichResult, what string, err error) transforms.EnrichResult {
	res.Errors++
	res.Notes = append(res.Notes, what+": "+err.Error())
	return res
}

// ---------------------------------------------------------------- claude

// Claude reads oauthAccount out of ~/.claude.json, not ~/.claude/.credentials.json: on macOS
// the tokens live in the keychain and that file does not exist.
type Claude struct{}

func NewClaude() *Claude { return &Claude{} }

func (*Claude) ID() string    { return "claude-account" }
func (*Claude) Version() int  { return 1 }
func (*Claude) Table() string { return "" }

// The auth store changes on login, not with transcripts: gating on units would mean an idle
// but logged-in install never reports its account. False for every probe here.
func (*Claude) NeedsUnits() bool { return false }
func (*Claude) Keyspaces() []string {
	return []string{"oauthAccount: emailAddress, organizationType, organizationRateLimitTier, " +
		"userRateLimitTier, seatTier, billingType, organizationName, organizationRole"}
}

func (*Claude) DBCandidates() []string {
	// ~/.claude.json sits above the agent root, so no glob under the root can reach it.
	return []string{"~/.claude.json"}
}

func (e *Claude) Enrich(in transforms.Input) transforms.EnrichResult {
	res := transforms.EnrichResult{EnricherID: e.ID(), Version: e.Version()}
	if in.DBPath == "" {
		return skippedAll(res, in, "no ~/.claude.json found: no account to report")
	}
	raw, err := readStore(in.DBPath)
	if err != nil {
		return failed(res, "claude account store unreadable", err)
	}
	var doc struct {
		OauthAccount struct {
			EmailAddress              string `json:"emailAddress"`
			OrganizationType          string `json:"organizationType"`
			OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
			UserRateLimitTier         string `json:"userRateLimitTier"`
			SeatTier                  string `json:"seatTier"`
			BillingType               string `json:"billingType"`
			OrganizationName          string `json:"organizationName"`
			OrganizationRole          string `json:"organizationRole"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return failed(res, "claude account store is not JSON", err)
	}
	a := doc.OauthAccount
	if a.EmailAddress == "" && a.OrganizationType == "" {
		return skippedAll(res, in, "~/.claude.json has no oauthAccount: not logged in")
	}
	tier := a.UserRateLimitTier
	if tier == "" {
		tier = a.OrganizationRateLimitTier
	}
	emit(&res, in.DBPath, transforms.Hash(raw), Account{
		Agent:         "claude-code",
		Email:         a.EmailAddress,
		Plan:          a.OrganizationType,
		RateLimitTier: tier,
		BillingTy:     a.BillingType,
		OrgName:       a.OrganizationName,
		OrgRole:       a.OrganizationRole,
		SeatTier:      a.SeatTier,
	})
	return res
}

// ---------------------------------------------------------------- codex

// Codex reads ~/.codex/auth.json, where the plan and the email are claims inside the OAuth
// id_token; no token reaches the output struct.
type Codex struct{}

func NewCodex() *Codex { return &Codex{} }

func (*Codex) ID() string       { return "codex-account" }
func (*Codex) Version() int     { return 1 }
func (*Codex) Table() string    { return "" }
func (*Codex) NeedsUnits() bool { return false }
func (*Codex) Keyspaces() []string {
	return []string{"auth_mode; tokens.id_token claims: email, chatgpt_plan_type, " +
		"chatgpt_subscription_active_until"}
}

func (*Codex) DBCandidates() []string {
	return []string{"$CODEX_HOME/auth.json", "~/.codex/auth.json"}
}

func (e *Codex) Enrich(in transforms.Input) transforms.EnrichResult {
	res := transforms.EnrichResult{EnricherID: e.ID(), Version: e.Version()}
	if in.DBPath == "" {
		return skippedAll(res, in, "no auth.json found: no account to report")
	}
	raw, err := readStore(in.DBPath)
	if err != nil {
		return failed(res, "codex auth store unreadable", err)
	}
	var doc struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return failed(res, "codex auth store is not JSON", err)
	}
	acct := Account{Agent: "codex", AuthMode: doc.AuthMode}
	if doc.Tokens.IDToken != "" {
		var claims struct {
			Email string `json:"email"`
			Auth  struct {
				PlanType    string `json:"chatgpt_plan_type"`
				ActiveUntil string `json:"chatgpt_subscription_active_until"`
			} `json:"https://api.openai.com/auth"`
		}
		if err := decodeJWTClaims(doc.Tokens.IDToken, &claims); err != nil {
			res.Errors++
			res.Notes = append(res.Notes, "codex id_token claims undecodable: "+err.Error())
			return res
		}
		acct.Email = claims.Email
		acct.Plan = claims.Auth.PlanType
		acct.ActiveUntil = claims.Auth.ActiveUntil
	}
	if acct.Email == "" && acct.Plan == "" && acct.AuthMode == "" {
		return skippedAll(res, in, "auth.json holds no account fields: not logged in")
	}
	emit(&res, in.DBPath, transforms.Hash(raw), acct)
	return res
}

// No verification: the token came from the agent's own login flow and only allowlisted claims
// survive into the output.
func decodeJWTClaims(tok string, dst any) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT: %d segments", len(parts))
	}
	pay, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	return json.Unmarshal(pay, dst)
}

// ---------------------------------------------------------------- cursor

// The cursorAuth keyspace is prefix-denied inside sqliteread because it also holds session
// tokens; these four keys are the compiled exact-key exceptions.
type Cursor struct{}

func NewCursor() *Cursor { return &Cursor{} }

func (*Cursor) ID() string       { return "cursor-account" }
func (*Cursor) Version() int     { return 1 }
func (*Cursor) Table() string    { return "ItemTable" }
func (*Cursor) NeedsUnits() bool { return false }
func (*Cursor) Keyspaces() []string {
	return []string{
		"cursorAuth/stripeMembershipType",
		"cursorAuth/cachedEmail",
		"cursorAuth/cachedSignUpType",
		"cursorAuth/cachedTeam",
	}
}

const cursorStateDB = "Cursor/User/globalStorage/state.vscdb"

func (*Cursor) DBCandidates() []string {
	// Same store, same platforms as cursor-transcript-join.
	switch runtime.GOOS {
	case "darwin":
		return []string{"~/Library/Application Support/" + cursorStateDB}
	case "linux":
		return []string{
			"~/.config/" + cursorStateDB,
			"~/.config/cursor/User/globalStorage/state.vscdb",
		}
	case "windows":
		return []string{"$APPDATA/" + cursorStateDB}
	default:
		return nil
	}
}

func (e *Cursor) Enrich(in transforms.Input) transforms.EnrichResult {
	res := transforms.EnrichResult{EnricherID: e.ID(), Version: e.Version()}
	if in.DBPath == "" {
		return skippedAll(res, in, "no state.vscdb found: no account to report")
	}
	read, err := sqliteread.Read(sqliteread.Options{
		Path:        in.DBPath,
		ScratchDir:  in.ScratchDir,
		Table:       e.Table(),
		KeyPrefixes: e.Keyspaces(),
	})
	if err != nil {
		return failed(res, "state.vscdb unreadable", err)
	}
	acct := Account{Agent: "cursor"}
	var prov []byte
	for _, row := range read.Rows {
		prov = append(prov, row.Key...)
		prov = append(prov, '=')
		prov = append(prov, row.Value...)
		prov = append(prov, '\n')
	}
	for _, row := range read.Rows {
		val := strings.TrimSpace(string(row.Value))
		switch row.Key {
		case "cursorAuth/stripeMembershipType":
			acct.Plan = val
		case "cursorAuth/cachedEmail":
			acct.Email = val
		case "cursorAuth/cachedSignUpType":
			acct.AuthMode = val
		case "cursorAuth/cachedTeam":
			var team struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(row.Value, &team) == nil {
				acct.Team = team.Name
			}
		}
	}
	if acct.Email == "" && acct.Plan == "" {
		return skippedAll(res, in, "cursorAuth keys absent: not logged in")
	}
	emit(&res, in.DBPath, transforms.Hash(prov), acct)
	return res
}
