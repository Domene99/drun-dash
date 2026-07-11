package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"
)

// This file implements the read-only `spend` command, talking to the
// control-plane analytics endpoint described in openapi/v1.yaml:
//
//   - GET /analytics/spend  (querySpend) -> SpendReport
//
// It mirrors the `agents` command exactly: the same Bearer-token loading (via
// internal/clitoken), the same --server / env base-URL mechanism, the same flat
// Error decoding for non-2xx (a 400 for bad flags surfaces the server's Error),
// and the same not-logged-in behavior.

// spendGroup mirrors the SpendGroup schema: spend attributed to one dimension
// key. Tokens is a pointer so an absent count is distinguishable from zero.
type spendGroup struct {
	Key    string  `json:"key"`
	Amount float64 `json:"amount"`
	Tokens *int64  `json:"tokens,omitempty"`
}

// spendReport mirrors the SpendReport schema: a spend aggregation over a time
// window, broken down by the requested dimension.
type spendReport struct {
	From     string       `json:"from"`
	To       string       `json:"to"`
	Currency string       `json:"currency"`
	Total    float64      `json:"total"`
	Groups   []spendGroup `json:"groups"`
}

// spendCmd calls GET /analytics/spend and prints the total plus per-group rows,
// passing --from / --to / --group-by through as query params.
func spendCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	from := fs.String("from", "", "inclusive start of the query window (RFC 3339)")
	to := fs.String("to", "", "exclusive end of the query window (RFC 3339)")
	groupBy := fs.String("group-by", "", "dimension to group spend by: agent|policy|principal")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash spend [flags]

Query aggregated LLM spend over a time window via GET /analytics/spend, using
the stored session token.

Flags:
  --from RFC3339      inclusive start of the query window
  --to RFC3339        exclusive end of the query window
  --group-by DIM      dimension to group spend by: agent|policy|principal
  --server URL        control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tok, ok := loadToken("spend", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	report, err := fetchSpendReport(hc, base, tok.AccessToken, *from, *to, *groupBy)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash spend: %v\n", err)
		return 1
	}

	printSpendReport(stdout, report)
	return 0
}

// fetchSpendReport performs GET /analytics/spend with the Bearer token, passing
// from / to / group_by as query params when set. Invalid flag values surface as
// the server's flat Error (typically a 400).
func fetchSpendReport(hc *http.Client, base, token, from, to, groupBy string) (*spendReport, error) {
	u := base + "/analytics/spend"
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	if groupBy != "" {
		q.Set("group_by", groupBy)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	body, status, err := getJSON(hc, u, token)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiErrorFromResponse(status, body)
	}
	var report spendReport
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, fmt.Errorf("decode spend report: %w", err)
	}
	return &report, nil
}

// printSpendReport renders the window, total, and per-group rows as an aligned
// table.
func printSpendReport(w io.Writer, r *spendReport) {
	fmt.Fprintf(w, "Window:   %s → %s\n", r.From, r.To)
	fmt.Fprintf(w, "Total:    %.2f %s\n", r.Total, r.Currency)
	if len(r.Groups) == 0 {
		fmt.Fprintln(w, "\nNo spend groups in this window.")
		return
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tAMOUNT\tTOKENS")
	for _, g := range r.Groups {
		tokens := "-"
		if g.Tokens != nil {
			tokens = fmt.Sprintf("%d", *g.Tokens)
		}
		fmt.Fprintf(tw, "%s\t%.2f %s\t%s\n", g.Key, g.Amount, r.Currency, tokens)
	}
	tw.Flush()
}
