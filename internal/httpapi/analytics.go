package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/store"
)

// analyticsStore is the narrow slice of *[store.AnalyticsStore] the analytics
// handlers use. Naming it as an interface (rather than reaching for the
// concrete store) lets the tests inject an in-memory fake in place of a live
// database while the production path still runs the real aggregation SQL.
type analyticsStore interface {
	SpendByOrg(ctx context.Context, orgID string, from, to time.Time, groupBy string) (store.SpendSummary, error)
	BudgetUtilization(ctx context.Context, orgID string, now time.Time) ([]store.BudgetUsageRow, error)
}

// newAnalyticsStore builds the [analyticsStore] a handler operates through. It
// is the same store-construction seam as newAgentStore: a package-level var so
// tests can swap in a fake without a database; production leaves it at its
// default, which constructs the real store over the request's pool.
var newAnalyticsStore = func(q store.Querier) analyticsStore {
	return store.NewAnalyticsStore(q)
}

// apiSpendReport is the OpenAPI SpendReport response
// (openapi/v1.yaml #/components/schemas/SpendReport). Money is kept precise as
// pgtype.Numeric inside the store and converted to a JSON number (float64) only
// here at the API boundary, per the issue's spec notes.
type apiSpendReport struct {
	From     time.Time       `json:"from"`
	To       time.Time       `json:"to"`
	Currency string          `json:"currency"`
	Total    float64         `json:"total"`
	Groups   []apiSpendGroup `json:"groups"`
}

// apiSpendGroup is one bucket of a SpendReport (#/components/schemas/SpendGroup):
// the group key with its attributed spend and token count.
type apiSpendGroup struct {
	Key    string  `json:"key"`
	Amount float64 `json:"amount"`
	Tokens int64   `json:"tokens"`
}

// apiBudgetList is the OpenAPI BudgetList response
// (#/components/schemas/BudgetList): the configured budgets with their
// current-period utilisation.
type apiBudgetList struct {
	Items []apiBudget `json:"items"`
}

// apiBudget is one budget's current utilisation (#/components/schemas/Budget).
// A rule with no hard cap has an invalid (SQL NULL) limit in the store; it is
// surfaced as 0 here since the schema requires the field.
type apiBudget struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Limit    float64 `json:"limit"`
	Spent    float64 `json:"spent"`
	Currency string  `json:"currency"`
	Period   string  `json:"period"`
}

// numericToFloat converts a store pgtype.Numeric money value to the JSON number
// used at the API boundary. An invalid numeric (SQL NULL — e.g. a budget rule
// with no hard cap) maps to 0, matching the "treat NULL as 0 at the boundary"
// convention; exact decimals are preserved up to float64 precision.
func numericToFloat(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}
	return f.Float64
}

// parseRFC3339Param reads a required RFC 3339 timestamp query parameter. A
// missing or unparseable value returns an error the handler maps to 400.
func parseRFC3339Param(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, fmt.Errorf("missing %s", name)
	}
	return time.Parse(time.RFC3339, raw)
}

// querySpend implements GET /analytics/spend (operationId querySpend). It
// aggregates the calling organization's LLM spend over the half-open window
// [from, to) grouped by group_by (agent default, policy, or principal), per
// ADR-0002 §1 (org from the token). from and to are required RFC 3339
// timestamps; group_by is validated against the accepted dimensions here so the
// store never sees an invalid value.
func (d Deps) querySpend(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	from, err := parseRFC3339Param(r, "from")
	if err != nil {
		WriteValidationError(w, "from", "from must be an RFC 3339 timestamp")
		return
	}
	to, err := parseRFC3339Param(r, "to")
	if err != nil {
		WriteValidationError(w, "to", "to must be an RFC 3339 timestamp")
		return
	}

	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = store.SpendGroupByAgent
	}
	switch groupBy {
	case store.SpendGroupByAgent, store.SpendGroupByPolicy, store.SpendGroupByPrincipal:
	default:
		WriteValidationError(w, "group_by", "group_by must be one of agent, policy, principal")
		return
	}

	s := newAnalyticsStore(d.Pool)
	summary, err := s.SpendByOrg(r.Context(), claims.OrgID, from, to, groupBy)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	groups := make([]apiSpendGroup, 0, len(summary.Groups))
	for _, g := range summary.Groups {
		groups = append(groups, apiSpendGroup{
			Key:    g.Key,
			Amount: numericToFloat(g.Amount),
			Tokens: g.Tokens,
		})
	}
	WriteOK(w, apiSpendReport{
		From:     from,
		To:       to,
		Currency: summary.Currency,
		Total:    numericToFloat(summary.Total),
		Groups:   groups,
	})
}

// listBudgets implements GET /analytics/budgets (operationId listBudgets). It
// lists the calling organization's budget rules with the spend accumulated in
// each rule's current window (ADR-0002 §1, org from the token). The current
// window is anchored to now, evaluated server-side.
func (d Deps) listBudgets(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	s := newAnalyticsStore(d.Pool)
	rows, err := s.BudgetUtilization(r.Context(), claims.OrgID, time.Now().UTC())
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	items := make([]apiBudget, 0, len(rows))
	for _, row := range rows {
		items = append(items, apiBudget{
			ID:       row.BudgetID,
			Name:     row.Name,
			Limit:    numericToFloat(row.Limit),
			Spent:    numericToFloat(row.Spent),
			Currency: row.Currency,
			Period:   string(row.Period),
		})
	}
	WriteOK(w, apiBudgetList{Items: items})
}
