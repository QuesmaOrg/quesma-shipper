// A hosted install is a shipper install that lives in a directory instead of on a person's
// machine: the OTLP gateway keeps one per person and runs the ordinary pipeline for each. Nothing
// here reads the process environment, so many can live in one process.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
)

// SpoolVar is the one variable the hosted catalog's roots name.
const SpoolVar = "QUESMA_OTEL_SPOOL"

type Hosted struct {
	// Home is an empty directory: every machine source resolves under it and finds nothing.
	Home     string
	StateDir string
	Spool    string

	// ConfigFile is the operator's local layer for every hosted install, such as upload_targets. May be empty.
	ConfigFile string
}

func (h Hosted) env() sources.Env {
	return sources.Env{Home: h.Home, Lookup: func(name string) (string, bool) {
		if name == SpoolVar {
			return h.Spool, true
		}
		return "", false
	}}
}

// layers disables every source that is not the gateway's own: a local disable beats a remote enable.
func (h Hosted) layers(compiled *sources.Compiled) ([]config.LayeredDocument, error) {
	doc := &config.Document{}
	if h.ConfigFile != "" {
		layers, err := config.LoadLayers(config.Paths{User: h.ConfigFile})
		if err != nil {
			return nil, err
		}
		if len(layers) == 1 {
			doc = layers[0].Doc
		}
	}
	doc.StateDir = nil
	off := false
	for _, s := range compiled.Sources() {
		if s.Family != "otel" {
			doc.Sources = append(doc.Sources, config.SourceOverride{ID: s.ID, Enabled: &off})
		}
	}
	return []config.LayeredDocument{{Layer: config.LayerUser, Doc: doc}}, nil
}

// Enroll gives the install an identity and enrolls it with a multi-use grant. Enrolled already is success.
func (h Hosted) Enroll(ctx context.Context, server, grant, hostname string) error {
	unlock, err := controlplane.LockEnrollment(h.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := controlplane.LoadEnrollment(h.StateDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	unit, err := loadOrMintIdentity(h.StateDir)
	if err != nil {
		return err
	}
	resp, endpoint, priv, err := controlplane.EnrollManaged(ctx, h.StateDir, server, controlplane.EnrollRequest{
		InstallID:    unit.InstallID.String(),
		AgeRecipient: unit.Recipient().String(),
		Hostname:     hostname,
		Platform:     "otel-gateway/" + runtime.GOARCH,
		Grant:        grant,
	})
	if err != nil {
		return managedEnrollmentError(err)
	}
	rec := controlplane.Enrollment{
		InstallID:    unit.InstallID.String(),
		Organization: resp.Organization,
		Endpoint:     endpoint,
		DeviceKey:    controlplane.EncodeKey(priv),
		EnrolledAt:   controlplane.Now(),
	}
	if err := rec.Save(h.StateDir); err != nil {
		return err
	}
	return controlplane.ClearManagedEnrollmentAttempt(h.StateDir)
}

// Runtime fetches the organization's served config and assembles the install's pipeline.
func (h Hosted) Runtime(ctx context.Context, build Build) (*Runtime, error) {
	compiled, err := sources.LoadHosted()
	if err != nil {
		return nil, err
	}
	layers, err := h.layers(compiled)
	if err != nil {
		return nil, err
	}
	env := h.env()
	eff, paths, remote, err := resolveLayers(ctx, env, config.Paths{StateDir: h.StateDir}, layers, compiled, false)
	if err != nil {
		return nil, err
	}
	if eff.StateDir != h.StateDir {
		return nil, fmt.Errorf("hosted install: the resolved state directory %q is not the install's own", eff.StateDir)
	}
	return newFrom(build, eff, paths, remote, env)
}
