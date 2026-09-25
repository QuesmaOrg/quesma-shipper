package sources

import "github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"

type opencodeSessions struct{}

func (opencodeSessions) Discover(req Request) (Discovery, error) {
	return discoverSessions(req, sqliteread.OpenCodeSessions{})
}
