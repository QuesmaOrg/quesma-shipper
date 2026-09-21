package sources

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
)

// One observation per app-server RPC. A signed-out account skips the backend-backed reads, the
// same way the Claude collector skips its endpoints without a token.
func (p *Accounts) collectCodex(ctx context.Context, req Request) ([]accountObservation, bool) {
	home := req.Source.Root
	if !accountPathExists(home) {
		return nil, false
	}
	now := req.Now().UTC()
	account := accountObservation{Source: "codex.appserver.account", ObservedAt: now}
	limits := accountObservation{Source: "codex.appserver.rateLimits", ObservedAt: now}
	usage := accountObservation{Source: "codex.appserver.usage", ObservedAt: now}
	all := func(code string) []accountObservation {
		for _, obs := range []*accountObservation{&account, &limits, &usage} {
			if obs.Error == "" && obs.Body == nil {
				obs.Error = code
			}
		}
		return []accountObservation{account, limits, usage}
	}
	binary, ok := codexBinary(req.Env)
	if !ok {
		return all("codex_unavailable"), true
	}
	// A spawn or handshake failure leaves every observation empty; each then reports request_failed.
	_ = runCodexAppServer(ctx, binary, home, func(c *codexRPC) error {
		raw, err := c.call("account/read", map[string]bool{"refreshToken": false})
		account = codexResult(account, raw, err)
		var signedIn struct {
			Account json.RawMessage `json:"account"`
		}
		if account.Error == "" && (json.Unmarshal(raw, &signedIn) != nil || len(signedIn.Account) == 0 || string(signedIn.Account) == "null") {
			limits.Error, usage.Error = "credentials_unavailable", "credentials_unavailable"
			return nil
		}
		raw, err = c.call("account/rateLimits/read", nil)
		limits = codexResult(limits, raw, err)
		raw, err = c.call("account/usage/read", nil)
		usage = codexResult(usage, raw, err)
		return nil
	})
	return all("request_failed"), true
}

func codexResult(obs accountObservation, raw json.RawMessage, err error) accountObservation {
	var rpc *codexRPCError
	switch {
	case errors.As(err, &rpc):
		obs.Error = "rpc_error"
	case errors.Is(err, bufio.ErrTooLong):
		obs.Error = "response_too_large"
	case err != nil:
		obs.Error = "request_failed"
	case len(raw) > accountResponseLimit:
		obs.Error = "response_too_large"
	case !json.Valid(raw):
		obs.Error = "invalid_json"
	default:
		obs.Body = raw
	}
	return obs
}
