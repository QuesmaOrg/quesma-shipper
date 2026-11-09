package transforms_test

import (
	"slices"
	"testing"

	"filippo.io/age"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestSealPolicyTracksEffectiveScrubbingAndRecipientSets(t *testing.T) {
	alice, _ := age.GenerateX25519Identity()
	bob, _ := age.GenerateX25519Identity()
	readers := []age.Recipient{alice.Recipient(), bob.Recipient()}
	digest := func(cfg transforms.Config, recipients []age.Recipient) string {
		t.Helper()
		hash, err := transforms.SealPolicyDigest(cfg, recipients)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	cfg := transforms.DefaultConfig()
	want := digest(cfg, readers)
	cfg.RulePacks = append(cfg.RulePacks, cfg.RulePacks[0])
	slices.Reverse(cfg.RulePacks)
	if got := digest(cfg, []age.Recipient{bob.Recipient(), alice.Recipient(), alice.Recipient()}); got != want {
		t.Fatal("ordering or duplicates changed the seal policy")
	}
	if digest(transforms.DefaultConfig(), readers[:1]) == want {
		t.Fatal("removing a reader left the seal policy unchanged")
	}
	for name, change := range map[string]func(*transforms.Config){
		"packs":        func(c *transforms.Config) { c.RulePacks = c.RulePacks[1:] },
		"secret names": func(c *transforms.Config) { c.SecretKeyNames = append(c.SecretKeyNames, "HOUSE_SIG") },
		"exemptions":   func(c *transforms.Config) { c.Exemptions = map[string][]string{"*": {"house"}} },
		"drops":        func(c *transforms.Config) { c.Drops = map[string][]string{"*": {"house"}} },
		"entropy":      func(c *transforms.Config) { c.Entropy.MinLength++ },
		"username":     func(c *transforms.Config) { c.Username = "alice" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := transforms.DefaultConfig()
			change(&cfg)
			if digest(cfg, readers) == want {
				t.Fatal("changed scrub policy kept the same digest")
			}
		})
	}
}
