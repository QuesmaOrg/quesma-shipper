package sources

import "github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"

type hermesSessions struct{}

func (hermesSessions) Discover(req Request) (Discovery, error) {
	return discoverSessions(req, sqliteread.HermesSessions{})
}
