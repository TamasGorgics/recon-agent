package lifecycle

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// LinkedBreakIDsForTest exposes the production break/match linkage helper to
// the external integration test. It is test-only and does not widen the
// package API.
func LinkedBreakIDsForTest(ctx context.Context, tx pgx.Tx, matchID string) ([]string, error) {
	link, err := loadActiveMatchLink(ctx, tx, matchID)
	if err != nil {
		return nil, err
	}
	return consumedBreakIDs(ctx, tx, link)
}
