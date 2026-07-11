package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// identity mirrors the Identity schema in openapi/v1.yaml.
type identity struct {
	Subject        string   `json:"subject"`
	OrganizationID string   `json:"organization_id"`
	Email          string   `json:"email,omitempty"`
	Roles          []string `json:"roles"`
}

// whoamiCmd loads the stored token and reports the authenticated identity via
// GET /auth/whoami (getCurrentIdentity). With no stored token it prints
// "Not logged in." and exits non-zero.
func whoamiCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash whoami [flags]

Show the currently authenticated member by calling the control plane's
/auth/whoami endpoint with the stored session token.

Flags:
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	store, err := clitoken.Default()
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash whoami: %v\n", err)
		return 1
	}

	tok, err := store.Load()
	if err != nil {
		if errors.Is(err, clitoken.ErrNotFound) {
			fmt.Fprintln(stderr, "Not logged in. Run `drun-dash login` first.")
			return 1
		}
		fmt.Fprintf(stderr, "drun-dash whoami: %v\n", err)
		return 1
	}

	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	id, err := fetchIdentity(hc, base, tok.AccessToken)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash whoami: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Subject:      %s\n", id.Subject)
	fmt.Fprintf(stdout, "Organization: %s\n", id.OrganizationID)
	if id.Email != "" {
		fmt.Fprintf(stdout, "Email:        %s\n", id.Email)
	}
	fmt.Fprintf(stdout, "Roles:        %s\n", strings.Join(id.Roles, ", "))
	return 0
}

// fetchIdentity calls GET /auth/whoami with a Bearer token.
func fetchIdentity(hc *http.Client, base, token string) (*identity, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/auth/whoami", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var id identity
		if err := json.Unmarshal(body, &id); err != nil {
			return nil, fmt.Errorf("decode identity: %w", err)
		}
		return &id, nil
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("token rejected (401); run `drun-dash login` to re-authenticate")
	default:
		return nil, fmt.Errorf("whoami: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// firstNonEmpty returns the first non-empty string in vs, or "".
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
