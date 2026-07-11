package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"text/tabwriter"

	"github.com/Domene99/drun-dash/internal/matrix"
)

// This file wires the parallel matrix engine (internal/matrix) into the CLI as
// `drun-dash matrix run`. The orchestration logic lives in internal/matrix;
// here we only parse flags, load the stored session token, adapt the existing
// control-plane HTTP helpers (launchSession/terminateSession from
// session_launch.go) to the matrix.ControlPlane interface, and render the
// ranked result. The runner and scorer stay user-supplied shell commands — the
// objective function is the user's — so no model-calling client is hardcoded.

// stringList is a repeatable string flag (used for --agent).
type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return fmt.Sprint([]string(*s))
}

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// cliControlPlane adapts the CLI's control-plane HTTP helpers to
// matrix.ControlPlane. Each branch is an independent governed session.
type cliControlPlane struct {
	hc    *http.Client
	base  string
	token string
}

func (c *cliControlPlane) Launch(ctx context.Context, agentID string) (matrix.Branch, error) {
	resp, err := launchSession(c.hc, c.base, c.token, sessionLaunchRequest{AgentID: agentID})
	if err != nil {
		return matrix.Branch{}, err
	}
	return matrix.Branch{
		SessionID:     resp.SessionID,
		ProxyBaseURL:  resp.ProxyBaseURL,
		ProxyToken:    resp.ProxyToken,
		FanOutCeiling: resp.FanOutCeiling,
	}, nil
}

func (c *cliControlPlane) Terminate(ctx context.Context, sessionID string) error {
	return terminateSession(c.hc, c.base, c.token, sessionID)
}

// matrixCmd dispatches the `matrix` subcommands.
func matrixCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		matrixUsage(stderr)
		return 2
	}
	switch sub := args[0]; sub {
	case "run":
		return matrixRunCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		matrixUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "drun-dash matrix: unknown subcommand %q\n\n", sub)
		matrixUsage(stderr)
		return 2
	}
}

// matrixUsage writes help for the `matrix` command tree.
func matrixUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: drun-dash matrix <subcommand> [flags]

Fan a task across multiple governed sessions, score each branch, and promote the
winner (design §6.3). Each branch is an independent governed session, so
entitlement, policy, budget, and metering are enforced per branch.

Subcommands:
  run    Launch, run, score, rank, and terminate a matrix of branches

Run "drun-dash matrix run -h" for flags.
`)
}

// matrixRunCmd parses flags, builds the control-plane adapter, and runs the
// matrix engine, then prints the ranked table and the winner.
func matrixRunCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("matrix run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var agents stringList
	fs.Var(&agents, "agent", "agent id to run as a branch (repeatable; required)")
	runner := fs.String("runner", "", "shell command that performs the work for a branch (required)")
	score := fs.String("score", "", "shell command that prints a numeric score for a branch to stdout (required)")
	maxParallel := fs.Int("max-parallel", 0, "max concurrent branches (default: branch count, clamped to the smallest fan-out ceiling)")
	keepWinner := fs.Bool("keep-winner", false, "leave the winning branch's session alive instead of terminating it")
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash matrix run --agent <id> [--agent <id> ...] --runner <cmd> --score <cmd> [flags]

Launch one governed session per --agent branch, run --runner in each (with the
branch's proxy credentials in the environment), score each branch with --score,
promote the highest-scoring branch, and terminate every session at the end.

Each branch's runner and scorer are executed via 'sh -c' with these environment
variables set, so the runner routes model calls through the governed proxy and
the scorer can locate the branch's output:

  DRUN_PROXY_BASE_URL   the branch's governed LLM-proxy base URL
  DRUN_PROXY_TOKEN      the branch's scoped proxy token (secret)
  DRUN_SESSION_ID       the branch's session id
  DRUN_AGENT_ID         the branch's agent id

The scorer must print a single numeric score to stdout; higher wins.

Flags:
  --agent ID          agent id to run as a branch (repeatable; required)
  --runner CMD        shell command that performs the work for a branch (required)
  --score CMD         shell command that prints a numeric score to stdout (required)
  --max-parallel N    max concurrent branches (default: branch count, clamped to the smallest fan-out ceiling)
  --keep-winner       leave the winning branch's session alive instead of terminating it
  --server URL        control-plane base URL override (default: the server used at login)

Exits non-zero if all branches fail.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(agents) == 0 {
		fmt.Fprintln(stderr, "drun-dash matrix run: at least one --agent <id> is required")
		fs.Usage()
		return 2
	}
	if *runner == "" {
		fmt.Fprintln(stderr, "drun-dash matrix run: --runner <cmd> is required")
		fs.Usage()
		return 2
	}
	if *score == "" {
		fmt.Fprintln(stderr, "drun-dash matrix run: --score <cmd> is required")
		fs.Usage()
		return 2
	}

	tok, ok := loadToken("matrix run", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	cp := &cliControlPlane{hc: newAuthHTTPClient(), base: base, token: tok.AccessToken}
	cfg := matrix.Config{
		Agents:      []string(agents),
		RunnerCmd:   *runner,
		ScoreCmd:    *score,
		MaxParallel: *maxParallel,
		KeepWinner:  *keepWinner,
		Control:     cp,
		Exec:        matrix.ShellExec{},
	}

	res, runErr := matrix.Run(context.Background(), cfg)
	if res != nil {
		printMatrixResult(stdout, res)
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "drun-dash matrix run: %v\n", runErr)
		return 1
	}
	return 0
}

// printMatrixResult renders the ranked branch table, the winner, and any
// termination warnings.
func printMatrixResult(w io.Writer, res *matrix.Result) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tAGENT\tSESSION ID\tSCORE\tSTATUS")
	for i, r := range res.Ranked {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", i+1, r.Agent, dashIfEmpty(r.SessionID), scoreCell(r), r.Status)
	}
	tw.Flush()

	fmt.Fprintln(w)
	if res.Winner != nil {
		fmt.Fprintf(w, "Winner: %s (session %s, score %s)\n", res.Winner.Agent, res.Winner.SessionID, formatScore(res.Winner.Score))
		if res.Winner.Terminated {
			fmt.Fprintln(w, "All sessions terminated.")
		} else {
			fmt.Fprintln(w, "Winner session kept alive (--keep-winner); all others terminated.")
		}
	} else {
		fmt.Fprintln(w, "No winning branch — all branches failed.")
	}

	// Surface per-branch failure detail and any termination errors.
	for _, r := range res.Ranked {
		if r.Err != nil {
			fmt.Fprintf(w, "  %s: %v\n", r.Agent, r.Err)
		}
		if r.TermErr != nil {
			fmt.Fprintf(w, "  %s: terminate failed: %v\n", r.Agent, r.TermErr)
		}
	}
}

// scoreCell renders a branch's score, or "-" when it produced none.
func scoreCell(r matrix.BranchResult) string {
	if !r.Scored {
		return "-"
	}
	return formatScore(r.Score)
}

func formatScore(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
