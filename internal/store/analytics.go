package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// AnalyticsStore runs read-only aggregation queries over the append-only
// usage_events table and the budgets/budget_rules config. It backs the
// analytics endpoints (GET /analytics/spend, GET /analytics/budgets); the HTTP
// handlers that shape these rows into the OpenAPI responses live elsewhere.
//
// Unlike the other stores it owns no table of its own — every method is a
// GROUP BY / SUM read — but it follows the same conventions: a constructor over
// a [Querier], hand-written SQL, and errors normalised through [mapError].
type AnalyticsStore struct {
	db Querier
}

// NewAnalyticsStore returns an AnalyticsStore backed by db.
func NewAnalyticsStore(db Querier) *AnalyticsStore {
	return &AnalyticsStore{db: db}
}

// Spend group_by dimensions accepted by [AnalyticsStore.SpendByOrg]. group_by
// selects which key the per-group spend is bucketed under; an empty value
// defaults to agent.
const (
	SpendGroupByAgent     = "agent"
	SpendGroupByPolicy    = "policy"
	SpendGroupByPrincipal = "principal"
)

// SpendGroupRow is one bucket of a [SpendSummary]: the total cost and token
// count attributed to a single group key over the report window. Key is the
// grouping id as text — an agent id, a policy id, or a principal (session
// launched_by) id — and is empty for the bucket of rows whose grouping column
// is NULL (e.g. usage with no agent attribution).
type SpendGroupRow struct {
	Key    string         // grouping id as text ("" for the NULL bucket)
	Amount pgtype.Numeric // SUM(cost_amount) for the group (numeric(20,6))
	Tokens int64          // SUM(total_tokens) for the group
}

// SpendSummary is the aggregate result of [AnalyticsStore.SpendByOrg]: the
// org-wide spend total over the window plus the per-group breakdown. Total is
// the sum across every usage event in the window regardless of group_by, so for
// the policy/principal dimensions — whose groups only cover usage that joins to
// a session — Total may exceed the sum of Groups. Currency is the events'
// shared currency when the window uses exactly one, and "USD" otherwise (no
// rows, or a mix); cost amounts are summed as-is without conversion.
type SpendSummary struct {
	Total    pgtype.Numeric
	Currency string
	Groups   []SpendGroupRow
}

// spendGroupQueries maps a group_by dimension to the grouped SQL that computes
// its buckets. Each query takes ($1 org_id, $2 from, $3 to) and returns
// (key text, amount numeric, tokens bigint) ordered by spend descending, with
// the key as a stable tie-break. The agent dimension reads agent_id straight
// off usage_events; policy and principal join through sessions (an inner join,
// so usage with no session is excluded from the breakdown).
var spendGroupQueries = map[string]string{
	SpendGroupByAgent: `
		SELECT COALESCE(agent_id::text, '') AS key,
		       COALESCE(SUM(cost_amount), 0) AS amount,
		       COALESCE(SUM(total_tokens), 0) AS tokens
		FROM usage_events
		WHERE org_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY agent_id
		ORDER BY amount DESC, key`,
	SpendGroupByPolicy: `
		SELECT pv.policy_id::text AS key,
		       COALESCE(SUM(ue.cost_amount), 0) AS amount,
		       COALESCE(SUM(ue.total_tokens), 0) AS tokens
		FROM usage_events ue
		JOIN sessions s ON ue.session_id = s.id
		JOIN policy_versions pv ON s.policy_version_id = pv.id
		WHERE ue.org_id = $1 AND ue.occurred_at >= $2 AND ue.occurred_at < $3
		GROUP BY pv.policy_id
		ORDER BY amount DESC, key`,
	SpendGroupByPrincipal: `
		SELECT COALESCE(s.launched_by::text, '') AS key,
		       COALESCE(SUM(ue.cost_amount), 0) AS amount,
		       COALESCE(SUM(ue.total_tokens), 0) AS tokens
		FROM usage_events ue
		JOIN sessions s ON ue.session_id = s.id
		WHERE ue.org_id = $1 AND ue.occurred_at >= $2 AND ue.occurred_at < $3
		GROUP BY s.launched_by
		ORDER BY amount DESC, key`,
}

// spendTotalQuery computes the org-wide total spend and a representative
// currency over the window. The currency is the events' single currency when
// they all agree, and 'USD' otherwise (including the empty window, where SUM is
// NULL → 0 and there are no currencies to inspect).
const spendTotalQuery = `
	SELECT COALESCE(SUM(cost_amount), 0) AS total,
	       CASE WHEN COUNT(DISTINCT currency) = 1 THEN MIN(currency) ELSE 'USD' END AS currency
	FROM usage_events
	WHERE org_id = $1 AND occurred_at >= $2 AND occurred_at < $3`

// SpendByOrg aggregates usage_events cost and tokens for orgID over the
// half-open window [from, to), grouped by groupBy — one of agent (default),
// policy, or principal. An empty groupBy defaults to agent; any other value is
// rejected with an error. The returned [SpendSummary] carries the org-wide
// Total (see its doc for how Total relates to the groups) and the per-group
// breakdown ordered by spend descending.
func (s *AnalyticsStore) SpendByOrg(ctx context.Context, orgID string, from, to time.Time, groupBy string) (SpendSummary, error) {
	if groupBy == "" {
		groupBy = SpendGroupByAgent
	}
	groupQuery, ok := spendGroupQueries[groupBy]
	if !ok {
		return SpendSummary{}, fmt.Errorf("store: spend by org %q: invalid group_by %q", orgID, groupBy)
	}

	var summary SpendSummary
	if err := s.db.QueryRow(ctx, spendTotalQuery, orgID, from, to).Scan(&summary.Total, &summary.Currency); err != nil {
		return SpendSummary{}, fmt.Errorf("store: spend total for org %q: %w", orgID, mapError(err))
	}

	rows, err := s.db.Query(ctx, groupQuery, orgID, from, to)
	if err != nil {
		return SpendSummary{}, fmt.Errorf("store: spend by %s for org %q: %w", groupBy, orgID, mapError(err))
	}
	summary.Groups, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (SpendGroupRow, error) {
		var g SpendGroupRow
		err := row.Scan(&g.Key, &g.Amount, &g.Tokens)
		return g, err
	})
	if err != nil {
		return SpendSummary{}, fmt.Errorf("store: spend by %s for org %q: %w", groupBy, orgID, mapError(err))
	}
	return summary, nil
}

// BudgetUsageRow is one budget rule's current-period utilisation, as computed
// by [AnalyticsStore.BudgetUtilization]: the budget's identity plus the rule's
// window and hard cap, alongside the spend accumulated so far in the current
// window. Limit is the rule's hard_cap and is invalid (Valid == false) when the
// rule sets no hard cap. Spent is always a valid numeric (0 when there is no
// usage in the window).
type BudgetUsageRow struct {
	BudgetID string         // budgets.id
	Name     string         // budgets.name
	Currency string         // budgets.currency
	Limit    pgtype.Numeric // budget_rules.hard_cap (invalid == no hard cap)
	Spent    pgtype.Numeric // SUM(cost_amount) over the current window
	Period   BudgetWindow   // budget_rules.window_period
}

// budgetUtilizationQuery returns one row per (budget, rule) for an org, with the
// rule's hard cap as the limit and the org's spend so far in the rule's current
// window. The window start is the calendar boundary for the rule's period
// (day/week/month) at $2 (now), computed with date_trunc; spend covers
// [window start, now]. Spend is aggregated org-wide — the rule's scope/scope_id
// narrowing is not yet applied here. Ordered by budget name then rule creation
// for a stable listing.
const budgetUtilizationQuery = `
	SELECT b.id,
	       b.name,
	       b.currency,
	       br.hard_cap,
	       br.window_period,
	       COALESCE((
	           SELECT SUM(ue.cost_amount)
	           FROM usage_events ue
	           WHERE ue.org_id = b.org_id
	             AND ue.occurred_at >= date_trunc(
	                 CASE br.window_period
	                     WHEN 'daily' THEN 'day'
	                     WHEN 'weekly' THEN 'week'
	                     WHEN 'monthly' THEN 'month'
	                 END, $2::timestamptz)
	             AND ue.occurred_at <= $2::timestamptz
	       ), 0) AS spent
	FROM budgets b
	JOIN budget_rules br ON br.budget_id = b.id
	WHERE b.org_id = $1
	ORDER BY b.name, br.created_at, br.id`

// BudgetUtilization returns, for every budget rule in orgID, the spend
// accumulated in that rule's current window (the daily/weekly/monthly calendar
// period containing now) against the rule's hard-cap limit. A budget with no
// rules contributes no rows; a budget with several rules contributes one row
// per rule, each sharing the budget's id/name/currency. Spend is aggregated
// org-wide for now — per-scope (team/agent/member) narrowing is not yet applied.
func (s *AnalyticsStore) BudgetUtilization(ctx context.Context, orgID string, now time.Time) ([]BudgetUsageRow, error) {
	rows, err := s.db.Query(ctx, budgetUtilizationQuery, orgID, now)
	if err != nil {
		return nil, fmt.Errorf("store: budget utilization for org %q: %w", orgID, mapError(err))
	}
	usage, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BudgetUsageRow, error) {
		var u BudgetUsageRow
		err := row.Scan(&u.BudgetID, &u.Name, &u.Currency, &u.Limit, &u.Period, &u.Spent)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: budget utilization for org %q: %w", orgID, mapError(err))
	}
	return usage, nil
}
