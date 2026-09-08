package app

// The two bridges between the control-plane client and the uploader. Here because app is the only
// package allowed to import both: internal/upload imports nothing internal, and the client may
// not depend on the data path.

import (
	"fmt"
	"maps"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/upload"
)

// uploadTargets turns the machine owner's allowlist into the matcher the uploader consults. One
// bad entry refuses the whole list, or the operator's file would disagree with the live origins.
func uploadTargets(eff *config.Effective) (upload.UploadTargetList, error) {
	list := make(upload.UploadTargetList, 0, len(eff.UploadTargets))
	for i, t := range eff.UploadTargets {
		target, err := upload.NewUploadTarget(upload.TargetSpec{
			Origin:            t.Origin,
			Addressing:        upload.Addressing(t.Addressing),
			PathPrefix:        t.PathPrefix,
			AllowLoopbackHTTP: t.AllowLoopbackHTTP,
		})
		if err != nil {
			return nil, fmt.Errorf("upload_targets entry %d: %w", i, err)
		}
		list = append(list, target)
	}
	return list, nil
}

// toUploadTicket copies one issued ticket into the uploader's shape after the control-plane client
// has rejected names outside the combined provider allowlist.
func toUploadTicket(t controlplane.Ticket) upload.Ticket {
	return upload.Ticket{
		TicketID:            t.TicketID,
		ObjectID:            t.ObjectID,
		Method:              t.Method,
		URL:                 t.URL,
		ExpiresAt:           t.ExpiresAt,
		RequiredHeaders:     maps.Clone(t.RequiredHeaders),
		ContentLength:       t.ContentLength,
		ContentLengthSigned: t.ContentLengthSigned,
	}
}
