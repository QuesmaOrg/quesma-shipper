package sources

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// One observation per app-server RPC. A signed-out account skips the backend-backed reads, the
// same way the Claude collector skips its endpoints without a token.
func (*Accounts) collectCodex(ctx context.Context, req Request) ([]accountObservation, bool) {
	home := req.Source.Root
	if !accountPathExists(home) {
		return nil, false
	}
	now := req.Now().UTC()
	obs := []accountObservation{
		{Source: "codex.appserver.account", ObservedAt: now},
		{Source: "codex.appserver.rateLimits", ObservedAt: now},
		{Source: "codex.appserver.usage", ObservedAt: now},
	}
	// Observations left empty by a spawn or handshake failure report the given code.
	fill := func(code string) []accountObservation {
		for i := range obs {
			if obs[i].Error == "" && obs[i].Body == nil {
				obs[i].Error = code
			}
		}
		return obs
	}
	binary, ok := codexBinary(req.Env)
	if !ok {
		return fill("codex_unavailable"), true
	}
	_ = runCodexAppServer(ctx, binary, home, func(c *codexRPC) error {
		raw, err := c.call("account/read", map[string]bool{"refreshToken": false})
		obs[0] = codexResult(obs[0], raw, err)
		var signedIn struct {
			Account json.RawMessage `json:"account"`
		}
		_ = json.Unmarshal(raw, &signedIn)
		if err == nil && (len(signedIn.Account) == 0 || bytes.Equal(signedIn.Account, []byte("null"))) {
			fill("credentials_unavailable")
			return nil
		}
		for i, method := range []string{"account/rateLimits/read", "account/usage/read"} {
			raw, err := c.call(method, nil)
			obs[i+1] = codexResult(obs[i+1], raw, err)
		}
		return nil
	})
	return fill("request_failed"), true
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
	default:
		obs = withBody(obs, raw)
	}
	return obs
}
