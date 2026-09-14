// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// Usage answers GET /v1/usage for the route of spec 011: q as the route
// parsed it, its key, model, and provider names already resolved to ids
// and the authorizer's filter already intersected through
// metering.Intersect. A range the query left open is the last day
// before now; a query outside the parameter table is a
// *metering.QueryError naming the parameter, which the route answers
// invalid_field. The rows are the store's, one per bucket, currency,
// and dimension tuple, and never nil.
func Usage(ctx context.Context, st store.Store, q metering.Query, now time.Time) ([]metering.Row, error) {
	q = q.WithDefaults(now)
	if err := q.Validate(); err != nil {
		return nil, err
	}
	rows, err := st.Usage().QueryRows(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("reading the usage rows from %s to %s: %w", q.From.UTC().Format(time.RFC3339), q.To.UTC().Format(time.RFC3339), err)
	}
	if rows == nil {
		rows = []metering.Row{}
	}
	return rows, nil
}
