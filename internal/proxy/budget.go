package proxy

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Domene99/drun-dash/internal/store"
)

// budgetExceededCode is the flat error code the broker emits when the calling
// org has hit a hard budget cap. Like the broker's other transport codes it is
// passed to httpx.WriteError as a plain string rather than added to the frozen
// httpx.Code… set, and it is paired with HTTP 429 (Too Many Requests) — the
// call is refused because the org has spent its allowance, not because the
// request itself is malformed.
const budgetExceededCode = "budget_exceeded"

// budgetReader is the broker's per-call budget lookup seam (ADR-0003 §5). It is
// an interface so tests can drive the under-cap and over-cap paths without a
// database; *store.AnalyticsStore satisfies it. BudgetUtilization aggregates the
// org's current-window spend against each budget rule's hard cap.
type budgetReader interface {
	BudgetUtilization(ctx context.Context, orgID string, now time.Time) ([]store.BudgetUsageRow, error)
}

// checkBudget resolves orgID's budget utilisation at now and returns the first
// budget rule whose current-window spend has reached or exceeded its hard cap
// (Spent >= Limit). hit reports whether such a cap was found; when it is, the
// returned row names the offending budget for the caller's error message.
//
// Rules with no hard cap (an invalid Limit — SQL NULL) are skipped: they impose
// no ceiling. Spent is always a valid numeric (0 when there is no usage in the
// window), so a rule with a hard cap of 0 blocks even at zero spend — a cap of
// zero means "allow nothing". The comparison is done in float64, matching the
// convention the analytics API boundary already uses for these money values
// (see internal/httpapi numericToFloat); numeric(20,6) amounts fall well within
// float64's exact-integer-plus-six-decimals range for realistic budgets.
//
// This is the v1 scope: caps are enforced at the org level using the existing
// org-wide aggregation. Per-member/agent/team scoped caps are a follow-up (the
// store already carries a rule scope; the aggregation does not yet narrow by
// it), as is a Redis running counter to avoid this per-call DB aggregation on
// the hot path.
func checkBudget(ctx context.Context, budgets budgetReader, orgID string, now time.Time) (row store.BudgetUsageRow, hit bool, err error) {
	rows, err := budgets.BudgetUtilization(ctx, orgID, now)
	if err != nil {
		return store.BudgetUsageRow{}, false, err
	}
	for _, r := range rows {
		limit, ok := numericValue(r.Limit)
		if !ok {
			continue // no hard cap on this rule
		}
		spent, _ := numericValue(r.Spent)
		if spent >= limit {
			return r, true, nil
		}
	}
	return store.BudgetUsageRow{}, false, nil
}

// numericValue converts a pgtype.Numeric money value to float64, reporting
// ok=false when the value is invalid (SQL NULL — e.g. a budget rule with no
// hard cap), matching internal/httpapi's numericToFloat except that it surfaces
// validity so the caller can distinguish "no cap" from a cap of zero.
func numericValue(n pgtype.Numeric) (float64, bool) {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0, false
	}
	return f.Float64, true
}
