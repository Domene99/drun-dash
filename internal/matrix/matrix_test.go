package matrix

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeControlPlane is a stand-in ControlPlane. It hands out a Branch per agent
// from launches (keyed by agent id) and records every Launch and Terminate call
// so tests can assert terminate-all behavior. launchErr fails a named agent's
// launch (e.g. a 403 not-entitled).
type fakeControlPlane struct {
	mu         sync.Mutex
	launches   map[string]Branch // agent -> branch to return
	launchErr  map[string]error  // agent -> error to return from Launch
	launched   []string          // agents launched, in order
	terminated []string          // session ids terminated, in order
}

func (f *fakeControlPlane) Launch(ctx context.Context, agentID string) (Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.launchErr[agentID]; err != nil {
		return Branch{}, err
	}
	f.launched = append(f.launched, agentID)
	br, ok := f.launches[agentID]
	if !ok {
		br = Branch{SessionID: "ses_" + agentID, ProxyBaseURL: "https://proxy/" + agentID, ProxyToken: "tok_" + agentID, FanOutCeiling: 4}
	}
	return br, nil
}

func (f *fakeControlPlane) Terminate(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, sessionID)
	return nil
}

// fakeExec is a stand-in Runner. It returns a scripted score per branch (keyed
// by DRUN_AGENT_ID), fails the runner or scorer for named agents, and tracks
// peak concurrency so tests can assert the fan-out clamp.
type fakeExec struct {
	scores   map[string]string // agent -> scorer stdout
	runErr   map[string]error  // agent -> runner error
	scoreErr map[string]error  // agent -> scorer error

	block chan struct{} // when non-nil, every call waits on it (to force overlap)

	mu       sync.Mutex
	active   int32
	maxActiv int32
	gotEnv   map[string]map[string]string // agent -> last env seen
}

func (e *fakeExec) Run(ctx context.Context, command string, env map[string]string) (string, error) {
	n := atomic.AddInt32(&e.active, 1)
	for {
		m := atomic.LoadInt32(&e.maxActiv)
		if n <= m || atomic.CompareAndSwapInt32(&e.maxActiv, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&e.active, -1)

	agent := env["DRUN_AGENT_ID"]
	e.mu.Lock()
	if e.gotEnv == nil {
		e.gotEnv = map[string]map[string]string{}
	}
	e.gotEnv[agent] = env
	e.mu.Unlock()

	if e.block != nil {
		<-e.block
	}

	// A command is the runner unless it is the score command marker "SCORE".
	isScore := command == "SCORE"
	if !isScore {
		if err := e.runErr[agent]; err != nil {
			return "", err
		}
		return "ran " + agent, nil
	}
	if err := e.scoreErr[agent]; err != nil {
		return "", err
	}
	s, ok := e.scores[agent]
	if !ok {
		s = "0"
	}
	return s, nil
}

func baseConfig(cp *fakeControlPlane, ex *fakeExec, agents ...string) Config {
	return Config{
		Agents:    agents,
		RunnerCmd: "RUN",
		ScoreCmd:  "SCORE",
		Control:   cp,
		Exec:      ex,
	}
}

func TestRunRanksAndPromotesWinner(t *testing.T) {
	cp := &fakeControlPlane{}
	ex := &fakeExec{scores: map[string]string{"a": "1.0", "b": "3.5", "c": "2.0"}}

	res, err := Run(context.Background(), baseConfig(cp, ex, "a", "b", "c"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Winner == nil || res.Winner.Agent != "b" {
		t.Fatalf("winner = %+v, want agent b", res.Winner)
	}
	if res.Winner.Score != 3.5 {
		t.Errorf("winner score = %v, want 3.5", res.Winner.Score)
	}
	// Ranked highest-first: b, c, a.
	gotOrder := []string{res.Ranked[0].Agent, res.Ranked[1].Agent, res.Ranked[2].Agent}
	if want := []string{"b", "c", "a"}; !equalStrs(gotOrder, want) {
		t.Errorf("ranked order = %v, want %v", gotOrder, want)
	}
	for _, r := range res.Ranked {
		if r.Status != StatusOK {
			t.Errorf("branch %s status = %q, want ok", r.Agent, r.Status)
		}
	}
	// Every session terminated (winner too — KeepWinner unset).
	if len(cp.terminated) != 3 {
		t.Errorf("terminated %d sessions, want 3 (%v)", len(cp.terminated), cp.terminated)
	}
	if !res.Winner.Terminated {
		t.Errorf("winner should be terminated when KeepWinner is unset")
	}
}

func TestRunKeepWinnerLeavesWinnerAlive(t *testing.T) {
	cp := &fakeControlPlane{}
	ex := &fakeExec{scores: map[string]string{"a": "1", "b": "9"}}

	cfg := baseConfig(cp, ex, "a", "b")
	cfg.KeepWinner = true
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Winner.Agent != "b" {
		t.Fatalf("winner = %s, want b", res.Winner.Agent)
	}
	// Loser terminated, winner kept.
	if len(cp.terminated) != 1 || cp.terminated[0] != "ses_a" {
		t.Errorf("terminated = %v, want only ses_a", cp.terminated)
	}
	if res.Winner.Terminated {
		t.Errorf("winner should be kept alive with KeepWinner")
	}
}

func TestRunFailedBranchDoesNotAbortOthers(t *testing.T) {
	cp := &fakeControlPlane{
		launchErr: map[string]error{"denied": errors.New("not entitled to launch agent \"denied\"")},
	}
	ex := &fakeExec{
		scores: map[string]string{"good": "5", "flaky": "0"},
		runErr: map[string]error{"flaky": errors.New("boom")},
	}

	res, err := Run(context.Background(), baseConfig(cp, ex, "good", "denied", "flaky"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Winner == nil || res.Winner.Agent != "good" {
		t.Fatalf("winner = %+v, want good", res.Winner)
	}

	byAgent := map[string]BranchResult{}
	for _, r := range res.Ranked {
		byAgent[r.Agent] = r
	}
	if s := byAgent["denied"].Status; s != StatusLaunchFailed {
		t.Errorf("denied status = %q, want launch-failed", s)
	}
	if byAgent["denied"].Err == nil || !strings.Contains(byAgent["denied"].Err.Error(), "not entitled") {
		t.Errorf("denied err = %v, want a not-entitled message", byAgent["denied"].Err)
	}
	if s := byAgent["flaky"].Status; s != StatusRunFailed {
		t.Errorf("flaky status = %q, want run-failed", s)
	}
	// launch-failed branch holds no session and is not terminated; the two
	// launched sessions (good + flaky) are.
	if len(cp.terminated) != 2 {
		t.Errorf("terminated = %v, want 2 (good + flaky)", cp.terminated)
	}
	if byAgent["denied"].Terminated {
		t.Errorf("launch-failed branch must not be terminated")
	}
	if !byAgent["flaky"].Terminated {
		t.Errorf("run-failed branch's session must still be terminated")
	}
}

func TestRunScoreParseFailureIsScoreFailed(t *testing.T) {
	cp := &fakeControlPlane{}
	ex := &fakeExec{scores: map[string]string{"a": "not-a-number", "b": "2"}}

	res, err := Run(context.Background(), baseConfig(cp, ex, "a", "b"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Winner.Agent != "b" {
		t.Errorf("winner = %s, want b", res.Winner.Agent)
	}
	byAgent := map[string]BranchResult{}
	for _, r := range res.Ranked {
		byAgent[r.Agent] = r
	}
	if byAgent["a"].Status != StatusScoreFailed {
		t.Errorf("a status = %q, want score-failed", byAgent["a"].Status)
	}
	// Both launched → both terminated.
	if len(cp.terminated) != 2 {
		t.Errorf("terminated = %v, want 2", cp.terminated)
	}
}

func TestRunFanOutClampBoundsConcurrency(t *testing.T) {
	cp := &fakeControlPlane{launches: map[string]Branch{
		"a": {SessionID: "ses_a", FanOutCeiling: 5},
		"b": {SessionID: "ses_b", FanOutCeiling: 2}, // smallest ceiling clamps to 2
		"c": {SessionID: "ses_c", FanOutCeiling: 5},
		"d": {SessionID: "ses_d", FanOutCeiling: 5},
	}}
	// block forces every call to overlap until we release, so maxActiv reflects
	// the true concurrency ceiling the engine allows.
	ex := &fakeExec{
		scores: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"},
		block:  make(chan struct{}),
	}
	// Release all blocked calls once the semaphore has saturated at the clamp.
	go func() {
		for atomic.LoadInt32(&ex.maxActiv) < 2 {
		}
		close(ex.block)
	}()

	// MaxParallel is generous (10); the smallest fan-out ceiling (2) must clamp.
	cfg := baseConfig(cp, ex, "a", "b", "c", "d")
	cfg.MaxParallel = 10
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&ex.maxActiv); got > 2 {
		t.Errorf("peak concurrency = %d, want <= 2 (clamped by smallest fan-out ceiling)", got)
	}
}

func TestRunMaxParallelBoundsConcurrency(t *testing.T) {
	cp := &fakeControlPlane{} // default ceiling 4
	ex := &fakeExec{
		scores: map[string]string{"a": "1", "b": "2", "c": "3"},
		block:  make(chan struct{}),
	}
	go func() {
		for atomic.LoadInt32(&ex.maxActiv) < 1 {
		}
		close(ex.block)
	}()

	cfg := baseConfig(cp, ex, "a", "b", "c")
	cfg.MaxParallel = 1 // serialize
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&ex.maxActiv); got > 1 {
		t.Errorf("peak concurrency = %d, want 1 with --max-parallel 1", got)
	}
}

func TestRunAllBranchesFailedReturnsError(t *testing.T) {
	cp := &fakeControlPlane{
		launchErr: map[string]error{
			"x": errors.New("not entitled"),
			"y": errors.New("not entitled"),
		},
	}
	ex := &fakeExec{}

	res, err := Run(context.Background(), baseConfig(cp, ex, "x", "y"))
	if !errors.Is(err, ErrAllBranchesFailed) {
		t.Fatalf("err = %v, want ErrAllBranchesFailed", err)
	}
	if res == nil || res.Winner != nil {
		t.Fatalf("winner = %+v, want nil when all failed", res.Winner)
	}
	// Nothing launched → nothing to terminate.
	if len(cp.terminated) != 0 {
		t.Errorf("terminated = %v, want none", cp.terminated)
	}
}

func TestRunAllRunnersFailStillTerminates(t *testing.T) {
	cp := &fakeControlPlane{}
	ex := &fakeExec{runErr: map[string]error{"a": errors.New("boom"), "b": errors.New("boom")}}

	res, err := Run(context.Background(), baseConfig(cp, ex, "a", "b"))
	if !errors.Is(err, ErrAllBranchesFailed) {
		t.Fatalf("err = %v, want ErrAllBranchesFailed", err)
	}
	if res.Winner != nil {
		t.Errorf("winner = %+v, want nil", res.Winner)
	}
	// Both launched, so both sessions must still be terminated even though all
	// runs failed.
	if len(cp.terminated) != 2 {
		t.Errorf("terminated = %v, want 2 despite all runs failing", cp.terminated)
	}
}

func TestRunPassesBranchEnvToCommands(t *testing.T) {
	cp := &fakeControlPlane{launches: map[string]Branch{
		"a": {SessionID: "ses_a", ProxyBaseURL: "https://p/a", ProxyToken: "ptok_a", FanOutCeiling: 3},
	}}
	ex := &fakeExec{scores: map[string]string{"a": "1"}}

	if _, err := Run(context.Background(), baseConfig(cp, ex, "a")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := ex.gotEnv["a"]
	want := map[string]string{
		"DRUN_PROXY_BASE_URL": "https://p/a",
		"DRUN_PROXY_TOKEN":    "ptok_a",
		"DRUN_SESSION_ID":     "ses_a",
		"DRUN_AGENT_ID":       "a",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
}

func TestRunValidatesConfig(t *testing.T) {
	cp := &fakeControlPlane{}
	ex := &fakeExec{}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no agents", Config{RunnerCmd: "r", ScoreCmd: "s", Control: cp, Exec: ex}},
		{"no runner", Config{Agents: []string{"a"}, ScoreCmd: "s", Control: cp, Exec: ex}},
		{"no score", Config{Agents: []string{"a"}, RunnerCmd: "r", Control: cp, Exec: ex}},
		{"no control", Config{Agents: []string{"a"}, RunnerCmd: "r", ScoreCmd: "s", Exec: ex}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), tc.cfg); err == nil {
				t.Errorf("Run(%s) err = nil, want a validation error", tc.name)
			}
		})
	}
}

func TestParseScore(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"42\n", 42, false},
		{"3.14", 3.14, false},
		{"-2.5\n", -2.5, false},
		{"noise\n7\n", 7, false},    // last non-empty line wins
		{"1\n\n  \n", 1, false},     // trailing blank lines ignored
		{"", 0, true},               // empty
		{"not-a-number\n", 0, true}, // non-numeric
	}
	for _, c := range cases {
		got, err := parseScore(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseScore(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("parseScore(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestShellExecPassesEnvThrough exercises the real ShellExec: it runs a `sh -c`
// command that echoes the injected branch env, proving the proxy env reaches
// the command's process. No sockets are bound (the sandbox blocks network).
func TestShellExecPassesEnvThrough(t *testing.T) {
	ex := ShellExec{}
	env := BranchEnv("a", Branch{SessionID: "ses_a", ProxyBaseURL: "https://proxy/a", ProxyToken: "secret"})

	out, err := ex.Run(context.Background(),
		`printf '%s|%s|%s|%s' "$DRUN_PROXY_BASE_URL" "$DRUN_PROXY_TOKEN" "$DRUN_SESSION_ID" "$DRUN_AGENT_ID"`, env)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "https://proxy/a|secret|ses_a|a"; out != want {
		t.Errorf("env passthrough = %q, want %q", out, want)
	}
}

func TestShellExecScorerStdoutParses(t *testing.T) {
	ex := ShellExec{}
	out, err := ex.Run(context.Background(), `echo 42`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v, err := parseScore(out); err != nil || v != 42 {
		t.Errorf("parseScore(%q) = %v, %v; want 42", out, v, err)
	}
}

func TestShellExecReportsFailure(t *testing.T) {
	ex := ShellExec{}
	if _, err := ex.Run(context.Background(), `echo oops >&2; exit 3`, nil); err == nil {
		t.Errorf("Run err = nil, want a non-zero exit error")
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
