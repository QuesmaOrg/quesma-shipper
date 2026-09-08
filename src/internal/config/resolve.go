package config

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/packs"
)

// ResolvedSource is sources.Resolved, aliased because this is the package that produces one.
type ResolvedSource = sources.Resolved

// Effective is the resolved configuration: the compiled ceiling, narrowed by every config layer in precedence order.
type Effective struct {
	ConfigVersion int

	// OrganizationID is the organization= key segment; standalone installs write the placeholder "default".
	OrganizationID string

	// ConfigExpired says the remote layer in force is a cached one past expiry; collection continues, expiry cannot widen scope.
	ConfigExpired bool

	Schedule string

	// DrainDeadline bounds `run --once --drain` and the SIGTERM drain, so an exit hook cannot block forever having flushed nothing.
	DrainDeadline time.Duration

	StateDir       string
	MaxFilesPerRun int
	Sources        []ResolvedSource

	// UploadTargets pins the origins a presigned upload ticket may name. Empty means unpinned; a bad entry is still refused.
	UploadTargets []UploadTarget

	RulePacks      []string
	SecretKeyNames []string
	StructuralEx   map[string][]string
	Deny           *sources.List

	// AdditionalRecipients are age public keys every object is encrypted to besides the install's own, kept as strings.
	AdditionalRecipients []string

	// IncludeInstallRecipient keeps the install's own key in the recipient set; withholding it requires another recipient.
	IncludeInstallRecipient bool

	// AutoupdateEnabled says a released build may replace itself at daemon startup; dev builds never self-update.
	AutoupdateEnabled bool

	// Provenance attributes every value to the layer that set it, for `config show --with-provenance`.
	Provenance map[string]Origin
}

// Origin is where one value came from.
type Origin struct {
	Layer Layer

	// Derived marks a value computed rather than configured; no layer set it.
	Derived bool
}

// SinkAdapter names the one write path. It is compiled in, and a send: block in a served document is discarded.
const SinkAdapter = "vend"

// UploadAddressings is the closed set of addressing forms, named so a refusal can print the alternatives.
var UploadAddressings = []string{"virtual-hosted", "path-style"}

// Input is everything a resolution needs; the remote layer arrives in Layers like any other document, as LayerRemote.
type Input struct {
	Catalog *sources.Compiled
	Layers  []LayeredDocument

	// ConfigExpired is set by the caller when the remote layer came from a stale cache; Resolve has no clock.
	ConfigExpired bool
	Env           sources.Env
	StateDir      string
}

// RejectionError is a config refusal. A rejected config is never partially applied: collection continues under the last valid one.
type RejectionError struct {
	Layer  Layer
	Field  string
	Reason string
}

func (e *RejectionError) Error() string {
	return fmt.Sprintf("config rejected: %s (set by the %s layer): %s", e.Field, e.Layer, e.Reason)
}

// Resolve merges the layers, records provenance, and enforces every limit.
func Resolve(in Input) (*Effective, error) {
	if in.Catalog == nil {
		return nil, errors.New("config: no compiled catalog")
	}

	eff := &Effective{
		ConfigVersion:  0,
		OrganizationID: "default",
		Schedule:       "15m",
		MaxFilesPerRun: 512,
		DrainDeadline:  5 * time.Minute,
		StateDir:       in.StateDir,
		ConfigExpired:  in.ConfigExpired,
		RulePacks:      []string{"gitleaks-core", "quesma-extra", "cloud-keys", "generic-entropy", "pii-core"},
		// Without the compiled exemption baseline the entropy backstop shreds the join keys that make a trajectory a graph.
		StructuralEx:            transforms.CompiledExemptions(),
		IncludeInstallRecipient: true,
		AutoupdateEnabled:       true,
		Provenance:              map[string]Origin{},
	}
	// Defaults are attributed too, so provenance never has a blank origin column.
	for _, k := range []string{"mode.schedule", "max_files_per_run",
		"send.sink", "scrub.rule_packs", "state_dir", "upload_targets", "config_version",
		"drain_deadline", "structural_exempt",
		"encryption.additional_recipients", "encryption.include_install_recipient",
		"autoupdate.enabled"} {
		eff.setOrigin(k, LayerCompiledDefaults)
	}

	// Source defaults come from the catalog, which is why it is layer 2.
	overrides := map[string][]layeredOverride{}
	for _, s := range in.Catalog.Sources() {
		eff.Sources = append(eff.Sources, ResolvedSource{
			Source:          s,
			Enabled:         s.IsEnabledByDefault(),
			SpecFingerprint: sources.SpecFingerprint(s),
		})
		eff.setOrigin("sources."+s.ID+".enabled", LayerBundledCatalog)
		eff.setOrigin("sources."+s.ID+".include", LayerBundledCatalog)
		eff.setOrigin("sources."+s.ID+".roots", LayerBundledCatalog)
	}

	// --- merge, lowest layer first ------------------------------------------
	for _, ld := range in.Layers {
		if ld.Doc == nil {
			continue
		}
		// The envelope describes the control plane's own issuing event, so a local file carrying it is a typo or a relabeling attempt.
		if ld.Doc.IssuedAt != nil || ld.Doc.Org != nil {
			if ld.Layer != LayerRemote {
				return nil, &RejectionError{ld.Layer, "issued_at / org",
					"only the served remote config carries the served envelope: these fields " +
						"describe the org's issuing event and its key namespace, not machine configuration"}
			}
			if ld.Doc.Org != nil && *ld.Doc.Org != "" {
				eff.OrganizationID = *ld.Doc.Org
				eff.setOrigin("organization", ld.Layer)
			}
		}

		if rej := applyDocument(eff, ld); rej != nil {
			return nil, rej
		}

		for _, o := range ld.Doc.Sources {
			if _, ok := in.Catalog.Source(o.ID); !ok {
				return nil, &RejectionError{ld.Layer, "sources." + o.ID,
					"no such source in the compiled catalog: a config layer cannot create a source, only adjust one"}
			}
			overrides[o.ID] = append(overrides[o.ID], layeredOverride{ld.Layer, o})
		}
	}

	if err := applySourceOverrides(eff, overrides); err != nil {
		return nil, err
	}

	// --- enforce ------------------------------------------------------------
	if err := checkConfigVersion(eff); err != nil {
		return nil, err
	}
	if err := checkUploadTargets(eff); err != nil {
		return nil, err
	}
	if err := checkRulePacks(eff); err != nil {
		return nil, err
	}
	if err := checkEncryption(eff); err != nil {
		return nil, err
	}

	eff.Deny = sources.New(in.Env.Home)

	if err := resolveRoots(eff, in); err != nil {
		return nil, err
	}

	slices.SortFunc(eff.Sources, func(a, b ResolvedSource) int { return cmp.Compare(a.ID, b.ID) })
	return eff, nil
}

// applyDocument folds one layer into the effective config: narrowing is free, widening stays with the machine owner.
func applyDocument(eff *Effective, ld LayeredDocument) *RejectionError {
	d, l := ld.Doc, ld.Layer

	if len(d.StructuralEx) > 0 {
		// Merged, never replaced: a layer's additions must not strip the compiled join-key protections underneath them.
		eff.StructuralEx = mergeExemptions(eff.StructuralEx, d.StructuralEx)
		eff.setOrigin("structural_exempt", l)
	}
	if d.ConfigVersion != nil {
		eff.ConfigVersion = *d.ConfigVersion
		eff.setOrigin("config_version", l)
	}
	if d.Mode != nil && d.Mode.Schedule != nil {
		eff.Schedule = *d.Mode.Schedule
		eff.setOrigin("mode.schedule", l)
	}
	if d.MaxFilesPerRun != nil {
		eff.MaxFilesPerRun = *d.MaxFilesPerRun
		eff.setOrigin("max_files_per_run", l)
	}
	if d.DrainDeadline != nil {
		v, err := time.ParseDuration(*d.DrainDeadline)
		if err != nil {
			return &RejectionError{l, "drain_deadline",
				fmt.Sprintf("%q is not a duration: %v", *d.DrainDeadline, err)}
		}
		if v <= 0 {
			return &RejectionError{l, "drain_deadline",
				"must be positive: a non-positive deadline makes every drain a no-op"}
		}
		eff.DrainDeadline = v
		eff.setOrigin("drain_deadline", l)
	}

	if d.Autoupdate != nil && d.Autoupdate.Enabled != nil {
		if *d.Autoupdate.Enabled && !l.IsLocal() {
			return &RejectionError{l, "autoupdate.enabled",
				"a non-local layer may turn self-update off but never on: re-enabling over a local refusal is a widen"}
		}
		eff.AutoupdateEnabled = *d.Autoupdate.Enabled
		eff.setOrigin("autoupdate.enabled", l)
	}
	if d.StateDir != nil {
		if !l.IsLocal() {
			return &RejectionError{l, "state_dir",
				"machine-owner only: it holds the identity unit, the fingerprints and the pause state"}
		}
		eff.StateDir = *d.StateDir
		eff.setOrigin("state_dir", l)
	}
	if len(d.UploadTargets) > 0 {
		if !l.IsLocal() {
			return &RejectionError{l, "upload_targets",
				"machine-owner only: a presigned ticket authorizes itself, so this pin is the only control on destinations"}
		}
		// Replace rather than union: two layers each holding half an allowlist would mean no file says where this machine writes.
		eff.UploadTargets = slices.Clone(d.UploadTargets)
		eff.setOrigin("upload_targets", l)
	}

	// Union: another rule pack or key name only makes scrubbing stricter, and no layer may remove another's readers.
	if s := d.Scrub; s != nil {
		if len(s.RulePacks) > 0 {
			eff.RulePacks = unionStrings(eff.RulePacks, s.RulePacks)
			eff.setOrigin("scrub.rule_packs", l)
		}
		if len(s.SecretKeyNames) > 0 {
			eff.SecretKeyNames = unionStrings(eff.SecretKeyNames, s.SecretKeyNames)
			eff.setOrigin("scrub.secret_key_names", l)
		}
	}
	if en := d.Encryption; en != nil {
		if len(en.AdditionalRecipients) > 0 {
			eff.AdditionalRecipients = unionStrings(eff.AdditionalRecipients, en.AdditionalRecipients)
			eff.setOrigin("encryption.additional_recipients", l)
		}
		if en.IncludeInstallRecipient != nil {
			eff.IncludeInstallRecipient = *en.IncludeInstallRecipient
			eff.setOrigin("encryption.include_install_recipient", l)
		}
	}
	return nil
}

type layeredOverride struct {
	layer    Layer
	override SourceOverride
}

// applySourceOverrides folds per-source overrides in layer order; a non-local layer cannot undo a local disable.
func applySourceOverrides(eff *Effective, overrides map[string][]layeredOverride) error {
	for i := range eff.Sources {
		src := &eff.Sources[i]
		locallyDisabledBy := Layer(0)
		// The enricher map is detached from the compiled catalog on first touch, once per source.
		clonedEnrichers := false

		for _, lo := range overrides[src.ID] {
			o := lo.override
			if o.Enabled != nil {
				if locallyDisabledBy != 0 && !lo.layer.IsLocal() && *o.Enabled {
					// A remote enable cannot undo a local disable.
					continue
				}
				src.Enabled = *o.Enabled
				eff.setOrigin("sources."+src.ID+".enabled", lo.layer)
				if !*o.Enabled && lo.layer.IsLocal() {
					locallyDisabledBy = lo.layer
				}
			}
			if len(o.Roots) > 0 {
				src.Roots = slices.Clone(o.Roots)
				eff.setOrigin("sources."+src.ID+".roots", lo.layer)
			}
			if len(o.Include) > 0 {
				src.Include = slices.Clone(o.Include)
				eff.setOrigin("sources."+src.ID+".include", lo.layer)
			}
			if len(o.Exclude) > 0 {
				src.Exclude = slices.Clone(o.Exclude)
				eff.setOrigin("sources."+src.ID+".exclude", lo.layer)
			}
			if o.MaxFileBytes != nil {
				src.MaxFileBytes = *o.MaxFileBytes
				eff.setOrigin("sources."+src.ID+".max_file_bytes", lo.layer)
			}
			for id, on := range o.Enrichers {
				if _, known := src.Enrichers[id]; !known {
					// Config may toggle only an enricher the catalog attached, never attach one.
					return &RejectionError{lo.layer, "sources." + src.ID + ".enrichers." + id,
						"the compiled catalog does not attach that enricher to this source"}
				}
				if on && !lo.layer.IsLocal() {
					// An enricher reads a database the raw pipeline never touches, so only a local layer may enable one.
					continue
				}
				// The map still points into the compiled catalog; mutating without a clone edits it for the life of the process.
				if !clonedEnrichers {
					src.Enrichers = maps.Clone(src.Enrichers)
					clonedEnrichers = true
				}
				src.Enrichers[id] = on
				eff.setOrigin("sources."+src.ID+".enrichers."+id, lo.layer)
			}
		}

		// Recomputed from the effective read-affecting fields, so a changed glob resets exactly this source's state.
		src.SpecFingerprint = sources.SpecFingerprint(src.Source)
		eff.setDerived("sources." + src.ID + ".spec_fingerprint")
		eff.setDerived("sources." + src.ID + ".root")
		eff.setOrigin("sources."+src.ID+".artifact_class", LayerBundledCatalog)
	}
	return nil
}

func checkConfigVersion(eff *Effective) error {
	if eff.ConfigVersion == 0 {
		// No layer stated one: the compiled default applies.
		eff.ConfigVersion = AcceptedConfigVersions[0]
		eff.setOrigin("config_version", LayerCompiledDefaults)
		return nil
	}
	if !slices.Contains(AcceptedConfigVersions, eff.ConfigVersion) {
		return &RejectionError{eff.Provenance["config_version"].Layer, "config_version",
			fmt.Sprintf("%d is not an accepted config_version %v", eff.ConfigVersion, AcceptedConfigVersions)}
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkUploadTargets refuses an entry that could not pin a destination, so a typo fails at `config show`, not at the first upload.
func checkUploadTargets(eff *Effective) error {
	layer := eff.Provenance["upload_targets"].Layer
	reject := func(format string, args ...any) error {
		return &RejectionError{layer, "upload_targets", fmt.Sprintf(format, args...)}
	}
	seen := map[string]bool{}
	for i, t := range eff.UploadTargets {
		if t.Origin == "" {
			return reject("entry %d names no origin", i)
		}
		u, err := url.Parse(t.Origin)
		switch {
		case err != nil, u.Host == "":
			return reject("entry %d: %q is not a scheme://host[:port] origin", i, t.Origin)
		case u.Opaque != "", u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "":
			return reject("entry %d: %q carries more than an origin: the bucket belongs in path_prefix", i, t.Origin)
		case u.User != nil:
			return reject("entry %d: %q carries user information", i, t.Origin)
		case strings.Contains(u.Hostname(), "*"):
			return reject("entry %d: %q is a wildcard host: a target is pinned or it is not a target", i, t.Origin)
		}
		switch {
		case u.Scheme == "https":
		case u.Scheme == "http" && t.AllowLoopbackHTTP && isLoopback(u.Hostname()):
		case u.Scheme == "http" && t.AllowLoopbackHTTP:
			return reject("entry %d: %q is http but %q is not loopback", i, t.Origin, u.Hostname())
		case u.Scheme == "http":
			return reject("entry %d: %q is http without allow_loopback_http: a presigned URL is a bearer credential", i, t.Origin)
		default:
			return reject("entry %d: %q uses scheme %q, want https", i, t.Origin, u.Scheme)
		}

		switch t.Addressing {
		case "virtual-hosted":
			if t.PathPrefix != "" {
				return reject("entry %d: virtual-hosted addressing declares path_prefix %q, but the "+
					"bucket is already the host", i, t.PathPrefix)
			}
		case "path-style":
			if !strings.HasPrefix(t.PathPrefix, "/") || t.PathPrefix == "/" || strings.HasSuffix(t.PathPrefix, "/") {
				return reject("entry %d: path-style addressing needs a /bucket path_prefix, got %q", i, t.PathPrefix)
			}
		default:
			return reject("entry %d: addressing %q is not one of %v", i, t.Addressing, UploadAddressings)
		}

		// One origin, one entry, with the port explicit so example and example:443 compare equal.
		port := cmp.Or(u.Port(), map[string]string{"http": "80", "https": "443"}[u.Scheme])
		key := strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
		if seen[key] {
			return reject("entry %d repeats origin %q", i, t.Origin)
		}
		seen[key] = true
	}
	return nil
}

func checkRulePacks(eff *Effective) error {
	// An unknown pack is refused once here rather than at transforms.New per file per tick.
	available := packs.Available()
	for _, name := range eff.RulePacks {
		if !slices.Contains(available, name) {
			return &RejectionError{eff.Provenance["scrub.rule_packs"].Layer, "scrub.rule_packs",
				fmt.Sprintf("%q is not a rule pack this build has %v", name, available)}
		}
	}
	return nil
}

// checkEncryption validates recipients at resolve time, where a bad config falls back; at seal time it would abort every flush.
func checkEncryption(eff *Effective) error {
	for _, r := range eff.AdditionalRecipients {
		if _, err := age.ParseX25519Recipient(r); err != nil {
			return &RejectionError{eff.Provenance["encryption.additional_recipients"].Layer,
				"encryption.additional_recipients",
				fmt.Sprintf("%q is not an age X25519 recipient: %v", r, err)}
		}
	}
	if !eff.IncludeInstallRecipient && len(eff.AdditionalRecipients) == 0 {
		return &RejectionError{eff.Provenance["encryption.include_install_recipient"].Layer,
			"encryption.include_install_recipient",
			"withholding the install recipient with no additional_recipients would seal objects no key can open"}
	}
	return nil
}

// resolveRoots expands each source's root candidates, then applies the deny list to the symlink-resolved path and require_subdir.
func resolveRoots(eff *Effective, in Input) error {
	compiled := in.Catalog

	for i := range eff.Sources {
		src := &eff.Sources[i]
		if !src.Enabled {
			continue
		}

		spec, _ := compiled.Source(src.ID)

		// The scope ceiling: a root must be one the compiled catalog declared for this source.
		for _, candidate := range src.Roots {
			if !slices.Contains(spec.Roots, candidate) {
				return &RejectionError{eff.Provenance["sources."+src.ID+".roots"].Layer,
					"sources." + src.ID + ".roots",
					fmt.Sprintf("%q is outside the compiled scope ceiling %v: a new root requires a release", candidate, spec.Roots)}
			}
		}

		var reasons []string
		for _, candidate := range src.Roots {
			expanded, err := in.Env.ExpandRoot(candidate)
			if err != nil {
				var unset *sources.ErrUnsetVar
				if errors.As(err, &unset) {
					reasons = append(reasons, unset.Error())
					continue
				}
				return &RejectionError{eff.Provenance["sources."+src.ID+".roots"].Layer,
					"sources." + src.ID + ".roots", err.Error()}
			}

			// Deny is checked before existence: a root pointed into ~/.ssh is a refusal whether or not it exists.
			if err := eff.Deny.CheckRoot(expanded); err != nil {
				return &RejectionError{eff.Provenance["sources."+src.ID+".roots"].Layer,
					"sources." + src.ID + ".roots", err.Error()}
			}

			// A missing root is the agent-absent case: expected silence, kept distinguishable from a root that matches nothing.
			info, statErr := os.Stat(expanded)
			if statErr != nil {
				reasons = append(reasons, fmt.Sprintf("%s does not exist", expanded))
				continue
			}
			if !info.IsDir() {
				reasons = append(reasons, fmt.Sprintf("%s is not a directory", expanded))
				continue
			}

			if err := requireSubdir(expanded, src.RequireSubdir); err != nil {
				reasons = append(reasons, err.Error())
				continue
			}
			if err := eff.Deny.CheckIncludes(expanded, src.Include); err != nil {
				return &RejectionError{eff.Provenance["sources."+src.ID+".include"].Layer,
					"sources." + src.ID + ".include", err.Error()}
			}

			src.Root = expanded
			break
		}
		if src.Root == "" {
			src.RootUnresolvedReason = strings.Join(reasons, "; ")
		}
	}
	return nil
}

// requireSubdir refuses a root without the declared subdirectory: a claimed store that does not look like one is not one.
func requireSubdir(root, subdir string) error {
	if subdir == "" {
		return nil
	}
	info, err := os.Stat(filepath.Join(root, subdir))
	if err != nil {
		return fmt.Errorf("root %s has no %s/ directory", root, subdir)
	}
	if !info.IsDir() {
		return fmt.Errorf("root %s: %s exists but is not a directory", root, subdir)
	}
	return nil
}

func (e *Effective) setOrigin(field string, l Layer) {
	e.Provenance[field] = Origin{Layer: l}
}

func (e *Effective) setDerived(field string) {
	e.Provenance[field] = Origin{Derived: true}
}

func unionStrings(base, add []string) []string {
	out := slices.Clone(base)
	for _, a := range add {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func mergeExemptions(base, add map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range base {
		out[k] = slices.Clone(v)
	}
	for k, v := range add {
		out[k] = unionStrings(out[k], v)
	}
	return out
}
