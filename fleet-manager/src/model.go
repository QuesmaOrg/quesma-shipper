package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const schemaVersion = 1

const quesmaETLAgeRecipient = "age1ge6plfgzzhzagl7qkp8k74qa90zxh4qvp84sgsw3up2zxrp5mq6qvex69a"

var orgPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type FleetConfig struct {
	Schema                  int       `json:"schema"`
	Organization            string    `json:"organization"`
	DisplayName             string    `json:"display_name,omitempty"`
	AgeRecipients           []string  `json:"age_recipients"`
	IncludeInstallRecipient bool      `json:"include_install_recipient"`
	AllowQuesmaETL          *bool     `json:"allow_quesma_etl,omitempty"`
	AuthoredYAML            string    `json:"authored_yaml"`
	UpdatedAt               time.Time `json:"updated_at"`
	TelemetryCollectorURL   *string   `json:"telemetry_collector_url,omitempty"`
}

// quesmaETLEnabled reports whether to seal to Quesma's recipient as well. Unset is on here, as in
// defaultOrganizationDefaults; a deployment that wants it off for organizations that never said
// says so in OrganizationDefaults, which the server applies before asking.
func (c FleetConfig) quesmaETLEnabled() bool {
	return c.AllowQuesmaETL == nil || *c.AllowQuesmaETL
}

// OrganizationDefaults is what an organization gets for a setting it never stated. A deployment
// names its own when it starts; defaultOrganizationDefaults is what it gets by naming nothing.
//
// Defaults are applied when a setting is used, never written into the record, so changing them
// changes every organization that left the setting unset, and none that stated it.
type OrganizationDefaults struct {
	AllowQuesmaETL        bool   `json:"allow_quesma_etl"`
	TelemetryCollectorURL string `json:"telemetry_collector_url"`
}

// resolve returns cfg with every unset setting filled from d. A stated value wins, including an
// explicit false or "".
func (d OrganizationDefaults) resolve(cfg FleetConfig) FleetConfig {
	if cfg.AllowQuesmaETL == nil {
		allow := d.AllowQuesmaETL
		cfg.AllowQuesmaETL = &allow
	}
	if cfg.TelemetryCollectorURL == nil {
		collector := d.TelemetryCollectorURL
		cfg.TelemetryCollectorURL = &collector
	}
	return cfg
}

// defaultOrganizationDefaults is what a deployment that states nothing gives its organizations.
//
// Quesma's recipient is on. Sealing cannot be added after the fact -- turning the recipient on later
// covers only what is uploaded from then on -- so an organization that may one day want Quesma's
// analytics over its history has to have been sealed to it from the start. It grants nothing by
// itself: Quesma still reads no object without a bucket grant, which defaults to none, and an
// organization or a deployment turns the recipient off by saying so.
//
// Telemetry is off: forwarding sends something out of the deployment, so it waits to be asked for.
func defaultOrganizationDefaults() OrganizationDefaults {
	return OrganizationDefaults{AllowQuesmaETL: true}
}

// organizationDefaultsFromEnv reads the deployment's defaults. What is unset keeps
// defaultOrganizationDefaults, and a value that does not parse stops the service rather than
// serving something nobody chose.
func organizationDefaultsFromEnv(getenv func(string) string) (OrganizationDefaults, error) {
	d := defaultOrganizationDefaults()
	if raw := strings.TrimSpace(getenv("FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL")); raw != "" {
		allow, err := strconv.ParseBool(raw)
		if err != nil {
			return d, fmt.Errorf("FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL must be true or false, not %q", raw)
		}
		d.AllowQuesmaETL = allow
	}
	d.TelemetryCollectorURL = strings.TrimSpace(getenv("FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL"))
	if _, err := normalizeTelemetryCollectorURL(d.TelemetryCollectorURL); err != nil {
		return d, fmt.Errorf("FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL: %w", err)
	}
	return d, nil
}

type AdminCredentialRecord struct {
	Schema       int    `json:"schema"`
	SecretDigest string `json:"secret_digest"`
}

type InstallStatus string

const (
	InstallPending InstallStatus = "pending"
	InstallActive  InstallStatus = "active"
	InstallRevoked InstallStatus = "revoked"
)

type InstallRecord struct {
	Schema           int           `json:"schema"`
	Organization     string        `json:"organization"`
	InstallID        string        `json:"install_id"`
	DevicePublicKey  string        `json:"device_public_key"`
	AgeRecipient     string        `json:"age_recipient"`
	Hostname         string        `json:"hostname,omitempty"`
	Platform         string        `json:"platform,omitempty"`
	EnrollmentDigest string        `json:"enrollment_digest"`
	Status           InstallStatus `json:"status"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	RevokedAt        *time.Time    `json:"revoked_at,omitempty"`
}

// SeenRecord is best-effort client telemetry, deliberately kept out of InstallRecord: the hot path
// must never rewrite a security record, and this one is disposable.
type SeenRecord struct {
	Schema        int        `json:"schema"`
	InstallID     string     `json:"install_id"`
	OS            string     `json:"os,omitempty"`
	BootedAt      string     `json:"booted_at,omitempty"`
	ClientVersion string     `json:"client_version,omitempty"`
	LastConfigAt  *time.Time `json:"last_config_at,omitempty"`
	LastVendAt    *time.Time `json:"last_vend_at,omitempty"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
}

// TagsRecord names an install for a human reader. It sits in the install's own root rather than
// under control/ so whatever reads the objects can read the name beside them, and it is separate
// from InstallRecord for the reason SeenRecord is: renaming a machine must not rewrite the record
// that carries its device key and revocation status.
type TagsRecord struct {
	Schema    int       `json:"schema"`
	InstallID string    `json:"install_id"`
	Name      string    `json:"name,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// credentialRecord is what a grant and an invite share. It is embedded first, so its fields encode
// first and flattened, exactly as when each record spelled them out.
type credentialRecord struct {
	Schema       int        `json:"schema"`
	ID           string     `json:"id"`
	SecretDigest string     `json:"secret_digest,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

func (r *credentialRecord) credential() *credentialRecord { return r }

type GrantRecord struct {
	credentialRecord
}

type InviteRecord struct {
	credentialRecord
	ReservedInstallID        string     `json:"reserved_install_id,omitempty"`
	ReservedEnrollmentDigest string     `json:"reserved_enrollment_digest,omitempty"`
	ReservedAt               *time.Time `json:"reserved_at,omitempty"`
	SpentAt                  *time.Time `json:"spent_at,omitempty"`
}

func tokenPrefix(org string) string        { return "fmi2." + org + "." }
func controlPrefix(org string) string      { return "v1/organization=" + org + "/control/" }
func configKey(org string) string          { return controlPrefix(org) + "config.json" }
func deploymentAdminCredentialKey() string { return "v1/control/admin/credential.json" }

// The reporter credential sits beside the administrator's and carries the same record: one digest,
// no lifecycle of its own. Terraform provisions both.
func deploymentReporterCredentialKey() string { return "v1/control/reporter/credential.json" }
func installKey(org, id string) string {
	return controlPrefix(org) + "installs/" + id + ".json"
}
func grantKey(org, id string) string { return controlPrefix(org) + "grants/" + id + ".json" }
func seenKey(org, id string) string  { return controlPrefix(org) + "seen/" + id + ".json" }
func healthKey(org, id string) string {
	return controlPrefix(org) + "health/" + id + ".json"
}
func inviteKey(org, id string) string {
	return controlPrefix(org) + "invites/" + id + ".json"
}

// The only key this service writes outside control/. Deliberate: a name is about the objects in
// that root, and a reader walking the install's prefix finds it without knowing the control layout.
func tagsKey(org, id string) string {
	return "v1/organization=" + org + "/install=" + id + "/tags.json"
}

func strictDecode[T any](raw []byte, out *T) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("more than one JSON value")
		}
		return err
	}
	return nil
}

func encodeRecord(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func validateConfig(c FleetConfig) error {
	if _, err := normalizeTelemetryCollectorURL(c.telemetryCollectorURL()); err != nil {
		return err
	}
	if c.Schema != schemaVersion {
		return fmt.Errorf("schema must be %d", schemaVersion)
	}
	if !orgPattern.MatchString(c.Organization) {
		return errors.New("organization must be a lowercase slug of at most 63 characters")
	}
	if c.DisplayName != "" {
		if err := validateDisplayName(c.DisplayName); err != nil {
			return err
		}
	}
	if len(c.AgeRecipients) == 0 && !c.IncludeInstallRecipient {
		return errors.New("configuration would leave installs with no encryption recipient")
	}
	seen := map[string]bool{}
	for _, recipient := range c.AgeRecipients {
		if recipient == quesmaETLAgeRecipient {
			return errors.New("the Quesma ETL recipient is managed by allow_quesma_etl")
		}
		if strings.HasPrefix(recipient, "AGE-SECRET-KEY-") {
			return errors.New("private age identities are forbidden")
		}
		parsed, err := age.ParseRecipients(strings.NewReader(recipient + "\n"))
		if err != nil {
			return fmt.Errorf("invalid public age recipient: %w", err)
		}
		if len(parsed) != 1 {
			return errors.New("each age recipient entry must contain exactly one public recipient")
		}
		if seen[recipient] {
			return errors.New("age recipients must be unique")
		}
		seen[recipient] = true
	}
	if strings.Contains(c.AuthoredYAML, "\n---") || strings.HasPrefix(c.AuthoredYAML, "---") {
		return errors.New("authored config must be a single YAML document")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(c.AuthoredYAML), &doc); err != nil {
		return fmt.Errorf("authored YAML: %w", err)
	}
	allowed := map[string]bool{"mode": true, "sources": true, "scrub": true, "max_files_per_run": true, "drain_deadline": true}
	for key := range doc {
		if !allowed[key] {
			return fmt.Errorf("authored YAML may not set %q", key)
		}
	}
	return nil
}

func validateDisplayName(name string) error { return validateHumanName("display name", name) }

func validateHumanName(label, name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return errors.New(label + " must be non-empty with no surrounding whitespace")
	}
	runes := []rune(name)
	if len(runes) > 100 {
		return errors.New(label + " must be at most 100 Unicode code points")
	}
	for _, r := range runes {
		if unicode.IsControl(r) {
			return errors.New(label + " must not contain control characters")
		}
	}
	return nil
}

func validateConfigForWrite(c FleetConfig) error {
	if err := validateConfig(c); err != nil {
		return err
	}
	if len(c.AgeRecipients) < 2 {
		return errors.New("at least two organization age recipients are required")
	}
	return nil
}

func mintToken(prefix string) (string, string, string, error) {
	id := uuid.NewString()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256(secret)
	return prefix + id + "." + encoded, id, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func verifyToken(token, prefix, id, digest string) bool {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return false
	}
	tokenID, secretText, ok := strings.Cut(rest, ".")
	if !ok || tokenID != id {
		return false
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretText)
	want, digestErr := base64.RawURLEncoding.DecodeString(digest)
	if err != nil || digestErr != nil || len(secret) != 32 || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256(secret)
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

func organizationToken(token string) (string, string, error) {
	rest, ok := strings.CutPrefix(token, "fmi2.")
	if !ok {
		return "", "", errors.New("invalid token")
	}
	secretDot := strings.LastIndexByte(rest, '.')
	if secretDot < 0 {
		return "", "", errors.New("invalid token")
	}
	beforeSecret := rest[:secretDot]
	idDot := strings.LastIndexByte(beforeSecret, '.')
	if idDot < 0 {
		return "", "", errors.New("invalid token")
	}
	org, id := beforeSecret[:idDot], beforeSecret[idDot+1:]
	if !orgPattern.MatchString(org) {
		return "", "", errors.New("invalid token")
	}
	parsed, err := uuidParse(id)
	if err != nil || parsed != id {
		return "", "", errors.New("invalid token")
	}
	return org, id, nil
}
