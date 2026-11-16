// Package gateway receives OTLP from Claude Code and Codex and spools it per person. It never
// uploads: the composition root hands it a Shipper, which runs the ordinary pipeline over a
// person's spool. Identity attributes are the client's claims; the ingest token only names the
// organization.
package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string        `yaml:"listen"`
	DataDir       string        `yaml:"data_dir"`
	FlushInterval time.Duration `yaml:"flush_interval"`
	MaxBodyBytes  int64         `yaml:"max_body_bytes"`

	// ShipperConfig is a shipper config.yaml applied to every hosted install as its local layer.
	ShipperConfig string `yaml:"shipper_config"`

	Tenants []Tenant `yaml:"tenants"`
}

type Tenant struct {
	Organization string `yaml:"organization"`
	FleetManager string `yaml:"fleet_manager"`

	// Grant is the multi-use enrollment grant each person's install enrolls with; GrantFile keeps it out of this file.
	Grant     string `yaml:"grant"`
	GrantFile string `yaml:"grant_file"`

	// TokenSHA256 holds the hex SHA-256 of each accepted ingest token. Several, so one can be rotated in.
	TokenSHA256 []string `yaml:"token_sha256"`

	// EmailDomains, when set, drops events whose claimed email is in another domain.
	EmailDomains []string `yaml:"email_domains"`

	// MaxPeople bounds how many installs one leaked token can mint.
	MaxPeople int `yaml:"max_people"`
}

var organizationName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func ParseConfig(raw []byte) (*Config, error) {
	c := Config{Listen: "127.0.0.1:4318", FlushInterval: 5 * time.Minute, MaxBodyBytes: 32 << 20}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("gateway config: %w", err)
	}
	if c.DataDir == "" {
		return nil, errors.New("gateway config: data_dir is required")
	}
	if c.FlushInterval <= 0 || c.MaxBodyBytes <= 0 {
		return nil, errors.New("gateway config: flush_interval and max_body_bytes must be positive")
	}
	seen := map[string]bool{}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if !organizationName.MatchString(t.Organization) || seen[t.Organization] {
			return nil, fmt.Errorf("gateway config: tenant %d: organization %q is invalid or repeated", i, t.Organization)
		}
		seen[t.Organization] = true
		if t.FleetManager == "" || len(t.TokenSHA256) == 0 {
			return nil, fmt.Errorf("gateway config: tenant %s: fleet_manager and token_sha256 are required", t.Organization)
		}
		for _, h := range t.TokenSHA256 {
			if raw, err := hex.DecodeString(h); err != nil || len(raw) != sha256.Size {
				return nil, fmt.Errorf("gateway config: tenant %s: token_sha256 holds a value that is not a SHA-256", t.Organization)
			}
		}
		if (t.Grant == "") == (t.GrantFile == "") {
			return nil, fmt.Errorf("gateway config: tenant %s: set exactly one of grant and grant_file", t.Organization)
		}
		if t.GrantFile != "" {
			raw, err := os.ReadFile(t.GrantFile)
			if err != nil {
				return nil, fmt.Errorf("gateway config: tenant %s: %w", t.Organization, err)
			}
			t.Grant = strings.TrimSpace(string(raw))
		}
		if t.MaxPeople == 0 {
			t.MaxPeople = 10000
		}
		for j, d := range t.EmailDomains {
			t.EmailDomains[j] = strings.ToLower(d)
		}
	}
	return &c, nil
}

// TokenPrefix marks an ingest token: qot1.<organization>.<secret>.
const TokenPrefix = "qot1."

// NewToken formats a token around a caller-made secret and returns what the config stores for it.
func NewToken(organization, secret string) (token, sha string) {
	token = TokenPrefix + organization + "." + secret
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:])
}

// tenantFor reads the organization from the right, since an organization may contain dots, then
// compares the whole token's hash in constant time.
func (c *Config) tenantFor(token string) *Tenant {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	dot := strings.LastIndexByte(rest, '.')
	if !ok || dot <= 0 {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	got := []byte(hex.EncodeToString(sum[:]))
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if t.Organization != rest[:dot] {
			continue
		}
		match := 0
		for _, h := range t.TokenSHA256 {
			match |= subtle.ConstantTimeCompare(got, []byte(strings.ToLower(h)))
		}
		if match == 1 {
			return t
		}
	}
	return nil
}

func (t *Tenant) admits(claims map[string]any) bool {
	email, _ := claims["user.email"].(string)
	if len(t.EmailDomains) == 0 || email == "" {
		return true
	}
	_, domain, _ := strings.Cut(strings.ToLower(email), "@")
	for _, d := range t.EmailDomains {
		if d == domain {
			return true
		}
	}
	return false
}
