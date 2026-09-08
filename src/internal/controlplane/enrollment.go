package controlplane

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// EnrollmentFile is where the enrollment record lives, beside the identity unit.
const EnrollmentFile = "enrollment.json"

// enrollmentSchema versions the record. A newer schema is rejected: guessing at one another version
// wrote is how a client writes into the wrong subtree. An OLDER one it knows is migrated forward and
// persisted, because at fleet scale "fix the file by hand" is no answer to a routine upgrade.
const enrollmentSchema = 2

// Enrollment is what an enrolled install remembers, persisted beside the identity unit and as one
// unit with it: without that identity it grants a subtree the client can no longer name.
type Enrollment struct {
	EnrollmentSchema int    `json:"enrollment_schema"`
	InstallID        string `json:"install_id"`
	Organization     string `json:"organization"`
	Endpoint         string `json:"endpoint"`

	// DeviceKey signs later requests. Private, so this file is 0600 and refused if it is looser.
	DeviceKey string `json:"device_key"`

	EnrolledAt string `json:"enrolled_at"`
}

// Save writes the record atomically at 0600.
func (e Enrollment) Save(stateDir string) error {
	e.EnrollmentSchema = enrollmentSchema
	return platform.WriteJSON(filepath.Join(stateDir, EnrollmentFile), e, 0o600)
}

// LoadEnrollment reads the record. A missing file means standalone, which is not an error. A
// record at an older schema the ladder still reaches is upgraded and persisted before returning.
func LoadEnrollment(stateDir string) (*Enrollment, error) {
	path := filepath.Join(stateDir, EnrollmentFile)
	// It holds a private signing key, so ReadPrivate's mode gate applies.
	raw, err := platform.ReadPrivate(path, 1<<20)
	if err != nil {
		return nil, err
	}

	raw, migrated, err := migrateEnrollment(raw)
	if err != nil {
		return nil, err
	}

	var e Enrollment
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("backend: parse enrollment: %w", err)
	}
	if e.EnrollmentSchema != enrollmentSchema {
		return nil, fmt.Errorf("backend: enrollment_schema %d, this client speaks %d",
			e.EnrollmentSchema, enrollmentSchema)
	}
	if migrated {
		// Persisted immediately so the upgrade happens once and a downgraded binary refuses the
		// record loudly instead of half-reading it. A failed persist is an error, not a shrug.
		if err := e.Save(stateDir); err != nil {
			return nil, fmt.Errorf("backend: persist migrated enrollment: %w", err)
		}
	}
	return &e, nil
}

// --- schema migrations -----------------------------------------------------------

// enrollmentMigrations climb one schema at a time: the entry at N takes the exact bytes a schema-N
// binary wrote and returns what a schema-N+1 binary would have written. Fixtures in testdata/ pin it.
var enrollmentMigrations = map[int]func([]byte) ([]byte, error){
	1: enrollmentV1toV2,
}

// migrateEnrollment walks the ladder up to the current schema. Bytes already current, not JSON, or
// naming a schema with no rung pass through untouched, for the caller's decode and check to judge.
func migrateEnrollment(raw []byte) (_ []byte, migrated bool, err error) {
	var probe struct {
		EnrollmentSchema int `json:"enrollment_schema"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.EnrollmentSchema == 0 {
		return raw, false, nil
	}
	for probe.EnrollmentSchema < enrollmentSchema {
		step, ok := enrollmentMigrations[probe.EnrollmentSchema]
		if !ok {
			return raw, false, nil
		}
		if raw, err = step(raw); err != nil {
			return nil, false, fmt.Errorf("backend: migrate enrollment from schema %d: %w",
				probe.EnrollmentSchema, err)
		}
		probe.EnrollmentSchema++
		migrated = true
	}
	return raw, migrated, nil
}

// enrollmentV1 is schema 1 FROZEN, exactly as the last schema-1 binary wrote it: never change it.
type enrollmentV1 struct {
	EnrollmentSchema int    `json:"enrollment_schema"`
	InstallID        string `json:"install_id"`
	Organization     string `json:"organization"`
	Endpoint         string `json:"endpoint"`
	DeviceKey        string `json:"device_key"`
	Sink             struct {
		Adapter string `json:"adapter"`
		Bucket  string `json:"bucket"`
		Region  string `json:"region"`
		KeyRoot string `json:"key_root"`
	} `json:"sink"`
	EnrolledAt string `json:"enrolled_at"`
}

// enrollmentV1toV2 drops the sink grant. It marshals the current struct because schema 2 IS
// current: the day schema 3 lands, this step must switch to a frozen v2 struct.
func enrollmentV1toV2(raw []byte) ([]byte, error) {
	var v1 enrollmentV1
	if err := json.Unmarshal(raw, &v1); err != nil {
		return nil, err
	}
	return json.Marshal(Enrollment{
		EnrollmentSchema: 2,
		InstallID:        v1.InstallID,
		Organization:     v1.Organization,
		Endpoint:         v1.Endpoint,
		DeviceKey:        v1.DeviceKey,
		EnrolledAt:       v1.EnrolledAt,
	})
}

// PrivateKey decodes the device signing key.
func (e Enrollment) PrivateKey() (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(e.DeviceKey)
	if err != nil {
		return nil, fmt.Errorf("backend: device key is not base64: %w", err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("backend: device key is %d bytes, want %d", len(b), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(b), nil
}

// NewDeviceKey generates the request-signing keypair.
func NewDeviceKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(nil)
}

// Now is the enrollment timestamp format.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// EncodeKey renders a key as base64, the wire and on-disk form for both halves.
func EncodeKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }
