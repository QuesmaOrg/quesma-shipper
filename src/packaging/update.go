// Package packaging owns installation, service registration, removal, and self-update.
package packaging

import (
	"context"
	"fmt"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

type UpdateOptions = common.Options
type UpdateResult = common.Result

const BrewUpgrade = common.BrewUpgrade
const BrewUninstall = common.BrewUninstall

func HomebrewManaged() bool { return common.HomebrewManaged() }

func CheckUpdate(ctx context.Context, o UpdateOptions) (string, time.Time, bool, error) {
	return common.Check(ctx, o)
}

func Update(ctx context.Context, o UpdateOptions) (UpdateResult, error) {
	if HomebrewManaged() {
		return UpdateResult{}, fmt.Errorf("Homebrew manages this installation; run `%s`", BrewUpgrade)
	}
	return common.Update(ctx, o, updateTarget, applyTarget)
}

func ReExec() error { return common.ReExec() }
