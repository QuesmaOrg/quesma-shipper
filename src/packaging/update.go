// Package packaging owns installation, service registration, removal, and self-update.
package packaging

import (
	"context"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

type UpdateOptions = common.Options
type UpdateResult = common.Result

func CheckUpdate(ctx context.Context, o UpdateOptions) (string, time.Time, bool, error) {
	return common.Check(ctx, o)
}

func Update(ctx context.Context, o UpdateOptions) (UpdateResult, error) {
	return common.Update(ctx, o, updateTarget, applyTarget)
}

func ReExec() error { return common.ReExec() }
