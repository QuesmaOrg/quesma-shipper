package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// ProvisioningFile is what an administrator's installer leaves for every user's first run: where to
// enroll and with which token. Any user on the machine can read it, so the token must be one the
// organisation is willing to hand every account there.
const ProvisioningFile = "provisioning.json"

const provisioningSchema = 1

type Provisioning struct {
	ProvisioningSchema int    `json:"provisioning_schema"`
	Server             string `json:"server"`
	Token              string `json:"token"`
}

// ReadProvisioning refuses a symlink itself, before trusted runs: the trust check follows links, and
// platform.ReadWhole's own refusal comes too late. A missing file is os.ErrNotExist.
func ReadProvisioning(path string, trusted func(string) error) (Provisioning, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Provisioning{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Provisioning{}, fmt.Errorf("provisioning: %s is a symlink", path)
	}
	if err := trusted(path); err != nil {
		return Provisioning{}, err
	}
	raw, _, err := platform.ReadWhole(path, 1<<16)
	if err != nil {
		return Provisioning{}, fmt.Errorf("provisioning: read %s: %w", path, err)
	}
	p, err := parseProvisioning(raw)
	if err != nil {
		return Provisioning{}, fmt.Errorf("%w (%s)", err, path)
	}
	return p, nil
}

func parseProvisioning(raw []byte) (Provisioning, error) {
	var p Provisioning
	if err := json.Unmarshal(raw, &p); err != nil {
		return Provisioning{}, fmt.Errorf("provisioning: parse: %w", err)
	}
	if p.ProvisioningSchema != provisioningSchema {
		return Provisioning{}, fmt.Errorf("provisioning: provisioning_schema %d, this client speaks %d",
			p.ProvisioningSchema, provisioningSchema)
	}
	p.Server, p.Token = strings.TrimSpace(p.Server), strings.TrimSpace(p.Token)
	if p.Server == "" || p.Token == "" {
		return Provisioning{}, errors.New("provisioning: server and token are both required")
	}
	return p, nil
}
