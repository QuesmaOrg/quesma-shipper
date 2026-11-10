package transforms

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"filippo.io/age"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// SealPolicyDigest identifies the readers and effective scrub policy for resumable local resends.
func SealPolicyDigest(cfg Config, recipients []age.Recipient) (string, error) {
	readers := make([]string, 0, len(recipients))
	for _, r := range recipients {
		s, ok := r.(fmt.Stringer)
		if !ok {
			return "", fmt.Errorf("seal policy: recipient %T has no stable identity", r)
		}
		readers = append(readers, s.String())
	}
	canonical := func(values []string) []string {
		return slices.Compact(slices.Sorted(slices.Values(values)))
	}
	readers = canonical(readers)
	cfg.RulePacks = canonical(cfg.RulePacks)
	cfg.SecretKeyNames = canonical(cfg.SecretKeyNames)
	cfg.Exemptions = maps.Clone(cfg.Exemptions)
	for family, paths := range cfg.Exemptions {
		cfg.Exemptions[family] = canonical(paths)
	}
	rules := make(map[string]string, len(cfg.RulePacks))
	for _, pack := range cfg.RulePacks {
		digest, err := packs.Digest(pack)
		if err != nil {
			return "", err
		}
		rules[pack] = digest
	}
	// Bump the version when code-only scrub semantics change without changing these inputs.
	body, err := json.Marshal(struct {
		Version int
		Readers []string
		Scrub   Config
		Rules   map[string]string
	}{1, readers, cfg, rules})
	if err != nil {
		return "", err
	}
	return Hash(body), nil
}
