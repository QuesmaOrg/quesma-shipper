package transforms_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

const recipientsVectorPath = "../../conformance/v1/seal/recipients.json"

// recipientsVector is the multi-recipient contract. age is nondeterministic by design, so there
// are no pinned bytes and every field is asserted behaviorally against the real Seal/Open.
type recipientsVector struct {
	VectorSet                 string `json:"vector_set"`
	VectorVersion             int    `json:"vector_version"`
	Description               string `json:"description"`
	Scheme                    string `json:"scheme"`
	KeyIDSource               string `json:"key_id_source"`
	KeyIDOrder                string `json:"key_id_order"`
	MinRecipients             int    `json:"min_recipients"`
	AnySingleIdentitySuffices bool   `json:"any_single_identity_suffices"`
}

func TestConformanceRecipients(t *testing.T) {
	raw, err := os.ReadFile(recipientsVectorPath)
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	var v recipientsVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}

	install, org, escrow := identity(t), identity(t), identity(t)
	recipients := []age.Recipient{install.Recipient(), org.Recipient(), escrow.Recipient()}
	payload := []byte("{\"type\":\"user\"}\n")

	obj, err := transforms.Seal(manifest(), payload, recipients)
	if err != nil {
		t.Fatal(err)
	}

	// key_id_order: the manifest records the set in seal argument order, so an auditor sees exactly
	// what the seal was told.
	m, _, err := transforms.Open(obj, install)
	if err != nil {
		t.Fatal(err)
	}
	if v.Scheme != "age" || m.Encryption == nil || m.Encryption.Scheme != v.Scheme {
		t.Fatalf("scheme: vector says %q, manifest says %+v", v.Scheme, m.Encryption)
	}
	wantIDs := []string{
		install.Recipient().String(), org.Recipient().String(), escrow.Recipient().String(),
	}
	if !slices.Equal(m.Encryption.RecipientKeyIDs, wantIDs) {
		t.Errorf("recipient_key_ids:\n got %v\nwant %v (seal argument order)",
			m.Encryption.RecipientKeyIDs, wantIDs)
	}

	// any_single_identity_suffices: the org reader and the escrow key each open the object alone,
	// which is what makes the ETL keyring work without any install's key.
	if !v.AnySingleIdentitySuffices {
		t.Fatal("the vector must claim any-single-identity: age's envelope construction guarantees it")
	}
	for name, id := range map[string]*age.X25519Identity{"org": org, "escrow": escrow} {
		if _, got, err := transforms.Open(obj, id); err != nil {
			t.Errorf("the %s identity alone must open the object: %v", name, err)
		} else if string(got) != string(payload) {
			t.Errorf("the %s identity read a different payload", name)
		}
	}

	// And an identity outside the set must not.
	if _, _, err := transforms.Open(obj, identity(t)); err == nil {
		t.Error("an identity that is not a recipient opened the object")
	}

	// min_recipients: encryption is not optional, so zero recipients is a refusal.
	if v.MinRecipients != 1 {
		t.Fatalf("min_recipients drifted: %d", v.MinRecipients)
	}
	if _, err := transforms.Seal(manifest(), payload, nil); err == nil {
		t.Error("sealing to no recipients must be refused")
	}
}
