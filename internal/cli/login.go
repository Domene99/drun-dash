package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// This file implements the client side of the OAuth 2.0 device-authorization
// grant (RFC 8628), per ADR 0001 §2. It speaks to the control-plane endpoints
// described in openapi/v1.yaml:
//
//   - POST /auth/device/code  (startDeviceAuthorization) -> DeviceAuthorizationResponse
//   - POST /auth/device/token (pollDeviceToken)          -> TokenResponse | OAuthError
//
// The resulting token is persisted via the clitoken package.

const (
	// serverEnv names the environment variable that supplies the control-plane
	// base URL when --server is not given.
	serverEnv = "DRUN_DASH_SERVER"
	// defaultServer is the fallback control-plane base URL (the local dev
	// stack's control-plane listen address).
	defaultServer = "http://localhost:8080"
	// defaultClientID identifies the CLI to the control plane's device
	// authorization endpoint.
	defaultClientID = "drun-dash-cli"
	// deviceCodeGrantType is the RFC 8628 grant type sent when polling.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// httpTimeout bounds each individual control-plane request. It is
	// deliberately per-request, not for the whole (potentially minutes-long)
	// poll loop.
	httpTimeout = 30 * time.Second
	// slowDownBackoff is the extra delay added to the poll interval when the
	// server responds with slow_down (RFC 8628 §3.5).
	slowDownBackoff = 5 * time.Second
	// defaultInterval is used when the server omits a positive interval.
	defaultInterval = 5 * time.Second
)

var (
	errDeviceCodeExpired = errors.New("device code expired before authorization was completed")
	errAccessDenied      = errors.New("authorization request was denied")
)

// --- request/response types, mirroring openapi/v1.yaml schemas ---

// deviceAuthRequest is the DeviceAuthorizationRequest body.
type deviceAuthRequest struct {
	ClientID string `json:"client_id"`
	Scope    string `json:"scope,omitempty"`
}

// deviceAuthResponse is the DeviceAuthorizationResponse body.
type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceTokenRequest is the DeviceTokenRequest body.
type deviceTokenRequest struct {
	ClientID   string `json:"client_id"`
	DeviceCode string `json:"device_code"`
	GrantType  string `json:"grant_type"`
}

// tokenResponse is the TokenResponse body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// oauthError is the OAuthError body (RFC 6749 / RFC 8628 style).
type oauthError struct {
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// clock abstracts wall-clock reads and sleeps so the poll loop can be tested
// deterministically without real delays.
type clock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

func realClock() clock {
	return clock{now: time.Now, sleep: time.Sleep}
}

// newAuthHTTPClient constructs the HTTP client used for control-plane auth
// calls. It is a package variable so tests can substitute an in-memory
// transport instead of binding a real socket; production always returns a
// plain *http.Client with a per-request timeout.
var newAuthHTTPClient = func() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// loginCmd runs the device-authorization flow and stores the resulting token.
func loginCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL (default $"+serverEnv+" or "+defaultServer+")")
	clientID := fs.String("client-id", defaultClientID, "registered CLI client identifier")
	scope := fs.String("scope", "", "space-delimited requested scopes")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash login [flags]

Authenticate this machine to the org via the OAuth 2.0
device-authorization flow (RFC 8628). Prints a verification URL and a
user code to enter in a browser, then waits for approval and stores the
resulting session token under the user config directory.

Flags:
  --server URL      control-plane base URL (default $`+serverEnv+` or `+defaultServer+`)
  --client-id ID    registered CLI client identifier (default `+defaultClientID+`)
  --scope SCOPES    space-delimited requested scopes
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	base := resolveServer(*server)
	store, err := clitoken.Default()
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash login: %v\n", err)
		return 1
	}

	hc := newAuthHTTPClient()
	clk := realClock()

	tok, err := deviceLogin(hc, base, *clientID, *scope, stdout, clk)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash login: %v\n", err)
		return 1
	}

	stored := clitoken.Token{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		ExpiresAt:    clk.now().Add(time.Duration(tok.ExpiresIn) * time.Second),
		RefreshToken: tok.RefreshToken,
		ServerURL:    base,
	}
	if err := store.Save(stored); err != nil {
		fmt.Fprintf(stderr, "drun-dash login: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "\nLogin successful. Token stored at %s\n", store.Path())
	return 0
}

// deviceLogin drives the full device flow: request a code, prompt the user,
// and poll until the request is approved, denied, or expires.
func deviceLogin(hc *http.Client, base, clientID, scope string, out io.Writer, clk clock) (*tokenResponse, error) {
	da, err := startDeviceAuth(hc, base, clientID, scope)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "To authorize this CLI, visit:\n\n    %s\n\n", da.VerificationURI)
	fmt.Fprintf(out, "and enter the code:\n\n    %s\n\n", da.UserCode)
	if da.VerificationURIComplete != "" {
		fmt.Fprintf(out, "Or open this URL to skip entering the code:\n\n    %s\n\n", da.VerificationURIComplete)
	}
	fmt.Fprintln(out, "Waiting for authorization...")

	wait := time.Duration(da.Interval) * time.Second
	if wait <= 0 {
		wait = defaultInterval
	}
	deadline := clk.now().Add(time.Duration(da.ExpiresIn) * time.Second)

	for {
		if !clk.now().Before(deadline) {
			return nil, errDeviceCodeExpired
		}

		tok, oerr, err := pollDeviceToken(hc, base, clientID, da.DeviceCode)
		if err != nil {
			return nil, err
		}
		if tok != nil {
			return tok, nil
		}

		switch oerr.ErrorCode {
		case "authorization_pending":
			// Keep polling at the current interval.
		case "slow_down":
			// RFC 8628 §3.5: back off by an additional interval.
			wait += slowDownBackoff
		case "access_denied":
			return nil, errAccessDenied
		case "expired_token":
			return nil, errDeviceCodeExpired
		default:
			if oerr.ErrorDescription != "" {
				return nil, fmt.Errorf("device authorization failed: %s (%s)", oerr.ErrorCode, oerr.ErrorDescription)
			}
			return nil, fmt.Errorf("device authorization failed: %s", oerr.ErrorCode)
		}

		clk.sleep(wait)
	}
}

// startDeviceAuth calls POST /auth/device/code.
func startDeviceAuth(hc *http.Client, base, clientID, scope string) (*deviceAuthResponse, error) {
	var out deviceAuthResponse
	if err := postJSON(hc, base+"/auth/device/code", deviceAuthRequest{ClientID: clientID, Scope: scope}, http.StatusOK, &out); err != nil {
		return nil, fmt.Errorf("start device authorization: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("start device authorization: incomplete response from server")
	}
	return &out, nil
}

// pollDeviceToken calls POST /auth/device/token once. It returns exactly one of:
// a token (approved, HTTP 200), an OAuth error (pending/slow_down/denied/
// expired, HTTP 400), or a transport/decoding error.
func pollDeviceToken(hc *http.Client, base, clientID, deviceCode string) (*tokenResponse, *oauthError, error) {
	body, err := json.Marshal(deviceTokenRequest{
		ClientID:   clientID,
		DeviceCode: deviceCode,
		GrantType:  deviceCodeGrantType,
	})
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/auth/device/token", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var tok tokenResponse
		if err := json.Unmarshal(respBody, &tok); err != nil {
			return nil, nil, fmt.Errorf("decode token response: %w", err)
		}
		if tok.AccessToken == "" {
			return nil, nil, fmt.Errorf("token endpoint returned an empty access token")
		}
		return &tok, nil, nil
	case http.StatusBadRequest:
		var oerr oauthError
		if err := json.Unmarshal(respBody, &oerr); err != nil {
			return nil, nil, fmt.Errorf("decode oauth error: %w", err)
		}
		if oerr.ErrorCode == "" {
			return nil, nil, fmt.Errorf("token endpoint returned HTTP 400 with no error code")
		}
		return nil, &oerr, nil
	default:
		return nil, nil, fmt.Errorf("token endpoint: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// postJSON marshals reqBody, POSTs it, and decodes a wantStatus response into
// out. Any other status is an error carrying the response body.
func postJSON(hc *http.Client, url string, reqBody any, wantStatus int, out any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// resolveServer picks the control-plane base URL from the flag value, then the
// environment, then the built-in default, trimming any trailing slash.
func resolveServer(flagVal string) string {
	v := flagVal
	if v == "" {
		v = os.Getenv(serverEnv)
	}
	if v == "" {
		v = defaultServer
	}
	return strings.TrimRight(v, "/")
}
