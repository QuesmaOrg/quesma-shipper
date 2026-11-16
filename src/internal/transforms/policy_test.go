package transforms_test

import (
	"encoding/json"
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

// A scrub-output change under the same policy would incorrectly reuse forced progress.
func TestSealPolicyPinsScrubConformanceOutputs(t *testing.T) {
	vectors := loadScrubVectors(t)
	cfg := transforms.DefaultConfig()
	cfg.Username, cfg.Exemptions = vectors.Username, vectors.Exemptions
	scrubber, err := transforms.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{}
	for _, vector := range vectors.Vectors {
		res, err := scrubber.Scrub([]byte(vector.Before), transforms.Hint{Family: vector.Family, JSONL: vector.JSONL})
		if err != nil {
			t.Fatal(err)
		}
		outputs[vector.Name] = string(res.Out.Bytes())
	}
	body, err := json.Marshal(outputs)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := transforms.SealPolicyDigest(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	const wantPolicy = "41622bf0416e8342bcf30b99246166049615a4c32d1ee4ae5c51550b9c0ec5ca"
	const wantOutput = "735afc073b37a38ca2dff0660e1e7dc07e1cc72efbbaa8955d72ee09614d27ca"
	if policy != wantPolicy || transforms.Hash(body) != wantOutput {
		t.Fatalf("scrub conformance changed: policy=%s output=%s; code-only scrub changes must bump Version in policy.go before recording a reviewed baseline", policy, transforms.Hash(body))
	}
}
