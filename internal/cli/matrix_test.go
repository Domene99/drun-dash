package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// matrixServer mocks the control-plane endpoints the matrix engine drives:
// POST /sessions/launch (one governed session per branch) and
// POST /sessions/{id}/terminate. It records launched agents and terminated
// session ids so tests can assert terminate-all behavior. forbid names agents
// whose launch returns 403 (not entitled).
type matrixServer struct {
	t          *testing.T
	fanOut     int
	forbid     map[string]bool
	mu         sync.Mutex
	launched   []string
	terminated []string
}

func (s *matrixServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			s.t.Errorf("method = %s, want POST", r.Method)
		}
		switch {
		case r.URL.Path == "/sessions/launch":
			var req sessionLaunchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				s.t.Errorf("decode launch body: %v", err)
			}
			if s.forbid[req.AgentID] {
				writeJSON(w, http.StatusForbidden, apiError{Code: "forbidden", Message: "no entitlement for " + req.AgentID})
				return
			}
			s.mu.Lock()
			s.launched = append(s.launched, req.AgentID)
			s.mu.Unlock()
			writeJSON(w, http.StatusCreated, sessionLaunchResponse{
				SessionID:     "ses_" + req.AgentID,
				ProxyBaseURL:  "https://proxy/" + req.AgentID,
				ProxyToken:    "ptok_" + req.AgentID,
				FanOutCeiling: s.fanOut,
			})
		case strings.HasPrefix(r.URL.Path, "/sessions/") && strings.HasSuffix(r.URL.Path, "/terminate"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/terminate")
			s.mu.Lock()
			s.terminated = append(s.terminated, id)
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, session{SessionID: id, Status: "terminated"})
		default:
			s.t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// scoreByAgent is a scorer that maps each branch's DRUN_AGENT_ID to a fixed
// score, so a test can pick a deterministic winner.
const scoreByAgent = `case "$DRUN_AGENT_ID" in
  agt_a) echo 1 ;;
  agt_b) echo 5 ;;
  agt_c) echo 3 ;;
  *) echo 0 ;;
esac`

func TestMatrixRunPromotesWinnerAndTerminatesAll(t *testing.T) {
	ms := &matrixServer{t: t, fanOut: 4}
	withAuthClient(t, mockClient(ms.handler()))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "mtok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"matrix", "run",
		"--agent", "agt_a", "--agent", "agt_b", "--agent", "agt_c",
		"--runner", "true",
		"--score", scoreByAgent,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	// agt_b (score 5) wins.
	if !strings.Contains(out.String(), "Winner: agt_b") {
		t.Errorf("stdout missing winner agt_b:\n%s", out.String())
	}
	// Ranked table shows all three branches with their scores.
	for _, want := range []string{"agt_a", "agt_b", "agt_c", "ses_agt_b"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
	// Every launched session terminated (winner too — no --keep-winner).
	if len(ms.terminated) != 3 {
		t.Errorf("terminated = %v, want 3", ms.terminated)
	}
	if !strings.Contains(out.String(), "All sessions terminated.") {
		t.Errorf("stdout missing terminate confirmation:\n%s", out.String())
	}
}

func TestMatrixRunKeepWinner(t *testing.T) {
	ms := &matrixServer{t: t, fanOut: 4}
	withAuthClient(t, mockClient(ms.handler()))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "mtok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"matrix", "run",
		"--agent", "agt_a", "--agent", "agt_b",
		"--runner", "true",
		"--score", scoreByAgent,
		"--keep-winner",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	// Only the loser (agt_a) is terminated; the winner (agt_b) is kept.
	if len(ms.terminated) != 1 || ms.terminated[0] != "ses_agt_a" {
		t.Errorf("terminated = %v, want only ses_agt_a", ms.terminated)
	}
	if !strings.Contains(out.String(), "kept alive") {
		t.Errorf("stdout should note the winner is kept alive:\n%s", out.String())
	}
}

func TestMatrixRunForbiddenBranchDoesNotAbortOthers(t *testing.T) {
	ms := &matrixServer{t: t, fanOut: 4, forbid: map[string]bool{"agt_c": true}}
	withAuthClient(t, mockClient(ms.handler()))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "mtok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"matrix", "run",
		"--agent", "agt_a", "--agent", "agt_b", "--agent", "agt_c",
		"--runner", "true",
		"--score", scoreByAgent,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	// agt_b still wins; the forbidden branch is reported but did not abort.
	if !strings.Contains(out.String(), "Winner: agt_b") {
		t.Errorf("stdout missing winner agt_b:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "launch-failed") {
		t.Errorf("stdout should show agt_c as launch-failed:\n%s", out.String())
	}
	if !strings.Contains(strings.ToLower(out.String()), "not entitled") {
		t.Errorf("stdout should surface the not-entitled reason:\n%s", out.String())
	}
	// Only the two entitled branches launched and were terminated.
	if len(ms.terminated) != 2 {
		t.Errorf("terminated = %v, want 2", ms.terminated)
	}
}

func TestMatrixRunAllForbiddenExitsNonZero(t *testing.T) {
	ms := &matrixServer{t: t, fanOut: 4, forbid: map[string]bool{"agt_a": true, "agt_b": true}}
	withAuthClient(t, mockClient(ms.handler()))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "mtok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"matrix", "run",
		"--agent", "agt_a", "--agent", "agt_b",
		"--runner", "true",
		"--score", "echo 1",
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when all branches fail", code)
	}
	if !strings.Contains(out.String(), "all branches failed") && !strings.Contains(errOut.String(), "all branches failed") {
		t.Errorf("want an 'all branches failed' message; stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestMatrixRunMissingAgent(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "mtok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"matrix", "run", "--runner", "true", "--score", "echo 1"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--agent") {
		t.Errorf("stderr = %q, want an --agent required message", errOut.String())
	}
}

func TestMatrixRunMissingRunnerAndScore(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "mtok", mockBase)

	// Missing --runner.
	var out, errOut bytes.Buffer
	if code := Run([]string{"matrix", "run", "--agent", "agt_a", "--score", "echo 1"}, &out, &errOut); code != 2 {
		t.Fatalf("missing runner: exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--runner") {
		t.Errorf("stderr = %q, want a --runner required message", errOut.String())
	}

	// Missing --score.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"matrix", "run", "--agent", "agt_a", "--runner", "true"}, &out, &errOut); code != 2 {
		t.Fatalf("missing score: exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--score") {
		t.Errorf("stderr = %q, want a --score required message", errOut.String())
	}
}

func TestMatrixRunNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"matrix", "run", "--agent", "agt_a", "--runner", "true", "--score", "echo 1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestMatrixUsageListedInTopLevelHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "matrix") {
		t.Errorf("top-level help missing matrix command:\n%s", out.String())
	}
}

func TestMatrixNoSubcommandShowsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"matrix"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "run") {
		t.Errorf("matrix usage missing 'run' subcommand:\n%s", errOut.String())
	}
}
