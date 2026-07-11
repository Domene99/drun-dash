// Package matrix implements the parallel matrix engine (design §6.3): fan a
// task across multiple governed sessions (the "branches"), run each in
// parallel, score every branch with an objective function, promote the winner,
// and terminate every session so no grant outlives the run.
//
// For this v1 CLI cut each branch is an independent governed session
// (POST /sessions/launch), which already enforces entitlement, policy, budget,
// and metering per branch. The drun checkpoint-fork optimization — forking all
// branches from one shared checkpoint instead of launching N sessions — is a
// documented follow-up.
//
// The orchestration is driven through two injectable interfaces — [ControlPlane]
// (launch/terminate) and [Runner] (execute a shell command for a branch) — so
// tests exercise the whole launch->run->score->rank->terminate flow with fakes,
// without real subprocesses or network. [Run] is the single entry point.
package matrix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Branch status values reported per branch in the ranked result. Exactly one
// is set on each [BranchResult].
const (
	// StatusOK marks a branch that launched, ran, and produced a numeric score.
	StatusOK = "ok"
	// StatusLaunchFailed marks a branch whose POST /sessions/launch failed
	// (e.g. a 403 not-entitled). It never held a session, so nothing is
	// terminated for it.
	StatusLaunchFailed = "launch-failed"
	// StatusRunFailed marks a branch whose --runner command failed. The branch
	// did launch, so its session is still terminated.
	StatusRunFailed = "run-failed"
	// StatusScoreFailed marks a branch whose --score command failed or printed
	// output that did not parse as a number. The session is still terminated.
	StatusScoreFailed = "score-failed"
)

// Branch is the result of launching one governed session — the fields the
// runner and scorer need to route model calls through the governed proxy, plus
// the fan-out ceiling used to clamp run concurrency.
type Branch struct {
	SessionID     string
	ProxyBaseURL  string
	ProxyToken    string
	FanOutCeiling int
}

// ControlPlane is the injectable control-plane client: one governed session per
// branch. Launch maps to POST /sessions/launch and Terminate to
// POST /sessions/{id}/terminate. A launch error fails only that branch; Run
// still launches, runs, and terminates the others.
type ControlPlane interface {
	Launch(ctx context.Context, agentID string) (Branch, error)
	Terminate(ctx context.Context, sessionID string) error
}

// Runner executes a single user-supplied shell command for one branch. The
// branch env (proxy base URL / token, session id, agent id) is passed so the
// command can route model calls through the governed proxy and locate the
// branch's output. Run returns the command's stdout, which the scorer step
// parses for a numeric score.
type Runner interface {
	Run(ctx context.Context, command string, env map[string]string) (stdout string, err error)
}

// Config is the input to [Run]. Agents, RunnerCmd, ScoreCmd, Control, and Exec
// are required.
type Config struct {
	// Agents are the branches — each an entitled agent/model id launched as its
	// own governed session.
	Agents []string
	// RunnerCmd performs the actual work for a branch; it is executed once per
	// branch with the branch env set.
	RunnerCmd string
	// ScoreCmd reads a branch's output and prints a numeric score to stdout;
	// higher wins.
	ScoreCmd string
	// MaxParallel bounds concurrent branch execution. <= 0 means "number of
	// branches". The effective bound never exceeds the smallest fan-out ceiling
	// returned across the launched sessions.
	MaxParallel int
	// KeepWinner leaves the winning branch's session alive at the end instead
	// of terminating it.
	KeepWinner bool

	Control ControlPlane
	Exec    Runner
}

// BranchResult is the per-branch outcome. Ranked results are returned in
// [Result].
type BranchResult struct {
	Agent     string
	SessionID string
	Score     float64
	Scored    bool
	Status    string
	// Err carries the failure detail for a non-ok branch (launch, run, or
	// score). It is nil for a successful branch.
	Err error
	// Terminated reports whether the branch's session was terminated at the end
	// of the run. It is false for a launch-failed branch (no session) and for a
	// kept winner (KeepWinner).
	Terminated bool
	// TermErr carries an error from the terminate call, if one occurred.
	TermErr error
}

// Result is the outcome of a matrix run: every branch ranked highest-score
// first, and a pointer to the winner (nil when no branch produced a score).
type Result struct {
	Ranked []BranchResult
	Winner *BranchResult
}

// ErrAllBranchesFailed is returned by [Run] when no branch produced a score.
var ErrAllBranchesFailed = errors.New("all branches failed")

// job pairs a launched branch with its slot in the results slice.
type job struct {
	idx    int
	agent  string
	branch Branch
}

// Run drives the full matrix: launch a governed session per agent, run and
// score the launched branches concurrently (bounded by MaxParallel and the
// smallest fan-out ceiling), rank them, and terminate every session (the winner
// too, unless KeepWinner). It always returns a [Result] describing every
// branch. It returns [ErrAllBranchesFailed] when no branch produced a score;
// the sessions are still terminated in that case.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if len(cfg.Agents) == 0 {
		return nil, errors.New("matrix: no agents specified")
	}
	if cfg.RunnerCmd == "" {
		return nil, errors.New("matrix: runner command is required")
	}
	if cfg.ScoreCmd == "" {
		return nil, errors.New("matrix: score command is required")
	}
	if cfg.Control == nil || cfg.Exec == nil {
		return nil, errors.New("matrix: control-plane and exec are required")
	}

	results := make([]BranchResult, len(cfg.Agents))

	// Launch phase: one governed session per agent. A failed launch fails only
	// that branch. The smallest fan-out ceiling across launched sessions clamps
	// run concurrency below.
	var jobs []job
	smallestCeiling := 0
	for i, agent := range cfg.Agents {
		results[i] = BranchResult{Agent: agent}
		br, err := cfg.Control.Launch(ctx, agent)
		if err != nil {
			results[i].Status = StatusLaunchFailed
			results[i].Err = err
			continue
		}
		results[i].SessionID = br.SessionID
		jobs = append(jobs, job{idx: i, agent: agent, branch: br})
		if br.FanOutCeiling > 0 && (smallestCeiling == 0 || br.FanOutCeiling < smallestCeiling) {
			smallestCeiling = br.FanOutCeiling
		}
	}

	// Run + score phase, bounded by the effective parallelism.
	parallel := effectiveParallel(cfg.MaxParallel, len(cfg.Agents), smallestCeiling)
	runBranches(ctx, cfg, jobs, results, parallel)

	// Pick the winner: the highest score among branches that produced one.
	winnerIdx := -1
	for i := range results {
		if results[i].Scored && (winnerIdx < 0 || results[i].Score > results[winnerIdx].Score) {
			winnerIdx = i
		}
	}

	// Terminate every launched session so no grant outlives the run — the
	// winner too, unless KeepWinner.
	for _, j := range jobs {
		if cfg.KeepWinner && j.idx == winnerIdx {
			continue
		}
		if err := cfg.Control.Terminate(ctx, j.branch.SessionID); err != nil {
			results[j.idx].TermErr = err
			continue
		}
		results[j.idx].Terminated = true
	}

	ranked := make([]BranchResult, len(results))
	copy(ranked, results)
	sort.SliceStable(ranked, func(i, k int) bool {
		if ranked[i].Scored != ranked[k].Scored {
			return ranked[i].Scored // scored branches rank above unscored ones
		}
		if ranked[i].Scored {
			return ranked[i].Score > ranked[k].Score
		}
		return false // preserve input order among failed branches
	})

	res := &Result{Ranked: ranked}
	if winnerIdx >= 0 {
		// The winner is ranked[0] by construction; return a pointer into Ranked
		// so callers see its final Terminated state.
		res.Winner = &res.Ranked[0]
		return res, nil
	}
	return res, ErrAllBranchesFailed
}

// runBranches executes the runner and scorer for each launched branch
// concurrently, bounded by parallel, writing each outcome into its results slot.
func runBranches(ctx context.Context, cfg Config, jobs []job, results []BranchResult, parallel int) {
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			score, scored, status, err := runBranch(ctx, cfg, j.agent, j.branch)
			results[j.idx].Score = score
			results[j.idx].Scored = scored
			results[j.idx].Status = status
			results[j.idx].Err = err
		}(j)
	}
	wg.Wait()
}

// runBranch executes the runner then the scorer for one branch and parses the
// numeric score. It returns the branch status and, on failure, a descriptive
// error.
func runBranch(ctx context.Context, cfg Config, agent string, br Branch) (score float64, scored bool, status string, err error) {
	env := BranchEnv(agent, br)
	if _, err := cfg.Exec.Run(ctx, cfg.RunnerCmd, env); err != nil {
		return 0, false, StatusRunFailed, fmt.Errorf("runner: %w", err)
	}
	out, err := cfg.Exec.Run(ctx, cfg.ScoreCmd, env)
	if err != nil {
		return 0, false, StatusScoreFailed, fmt.Errorf("score: %w", err)
	}
	s, err := parseScore(out)
	if err != nil {
		return 0, false, StatusScoreFailed, fmt.Errorf("score: %w", err)
	}
	return s, true, StatusOK, nil
}

// effectiveParallel resolves the concurrency bound: the requested value (or the
// branch count when unset), never above the smallest fan-out ceiling, never
// below 1.
func effectiveParallel(requested, branches, smallestCeiling int) int {
	p := requested
	if p <= 0 {
		p = branches
	}
	if smallestCeiling > 0 && p > smallestCeiling {
		p = smallestCeiling
	}
	if p < 1 {
		p = 1
	}
	return p
}

// BranchEnv builds the environment passed to the runner and scorer for a
// branch. DRUN_PROXY_BASE_URL / DRUN_PROXY_TOKEN route model calls through the
// governed proxy; DRUN_SESSION_ID / DRUN_AGENT_ID let a command name and locate
// the branch's output.
func BranchEnv(agent string, br Branch) map[string]string {
	return map[string]string{
		"DRUN_PROXY_BASE_URL": br.ProxyBaseURL,
		"DRUN_PROXY_TOKEN":    br.ProxyToken,
		"DRUN_SESSION_ID":     br.SessionID,
		"DRUN_AGENT_ID":       agent,
	}
}

// parseScore reads the numeric score from a scorer's stdout: the last non-empty
// line, parsed as a float. Higher wins.
func parseScore(out string) (float64, error) {
	var last string
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			last = t
		}
	}
	if last == "" {
		return 0, errors.New("empty score output")
	}
	v, err := strconv.ParseFloat(last, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", last)
	}
	return v, nil
}

// ShellExec is the production [Runner]: it runs each command via `sh -c` with
// the process environment plus the branch env. It is the default injected by
// the CLI; tests inject a fake instead.
type ShellExec struct{}

// Run executes command through `sh -c`, appending env to the inherited process
// environment, and returns its stdout. On failure the error includes any
// stderr for diagnosis.
func (ShellExec) Run(ctx context.Context, command string, env map[string]string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}
