package engine

import (
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// baseManifest fills the fields every object of this run shares, raw and derived alike, so a
// wire-contract field added once cannot silently miss one path. The hashes, size and recipient
// ids are Seal's to fill: it returns the manifest that actually landed.
func (o Options) baseManifest(src sources.Resolved, nativePath string) transforms.Manifest {
	return transforms.Manifest{
		ManifestVersion: transforms.ManifestVersion,
		OrganizationID:  orgOf(o.Plan),
		InstallID:       o.Identity.InstallID.String(),
		SourceID:        src.ID,
		SourceFamily:    src.Family,
		NativePath:      formats.ApplyUserPlaceholder(nativePath, o.user),
		Gather:          src.Gather,
		ArtifactClass:   src.ArtifactClass,
		SealedAt:        o.Now().Format(time.RFC3339),
		ConfigVersion:   o.Plan.ConfigVersion,
		// Stamped per object, so a reader can tell which objects were collected under a stale config.
		ConfigExpired: o.Plan.ConfigExpired,
		Client:        o.Client,
		RunID:         o.RunID,
	}
}

// mirrorKey is the key an object ships to. Path-derived, so a re-run lands on the same one.
func (o Options) mirrorKey(sourceID, relPath string) (string, error) {
	return formats.MirrorKey(orgOf(o.Plan), o.Identity.InstallID.String(), sourceID,
		o.Identity.NameKey, formats.CanonicalPath(relPath, o.user))
}

// Manifest construction for raw objects. The manifest is the only place a native path exists on
// the wire, so the username placeholder is applied here as well as in the payload.
func (o Options) manifestFor(
	src sources.Resolved,
	cand sources.Candidate,
	disc sources.Discovery,
	sourceHash string,
	mtime time.Time,
	res transforms.Result,
) transforms.Manifest {
	m := o.baseManifest(src, cand.Path)
	m.SourceHash = sourceHash
	m.PayloadMTime = &mtime
	m.AgentVersion = disc.AgentVersion
	m.ShapeSniff = string(disc.Sniff)
	if src.Scrub == nil || *src.Scrub {
		m.Redaction = &transforms.RedactionSummary{
			Density:  res.Density(),
			RuleHits: res.RuleHits,
			ScanMode: res.ScanMode,
		}
	}
	if m.ShapeSniff == "" {
		m.ShapeSniff = string(sources.SniffOK)
	}
	return m
}
