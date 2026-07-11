package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
)

// This file implements the read-only `policies` command tree, talking to the
// control-plane policy endpoints described in openapi/v1.yaml:
//
//   - GET /policies                        (listPolicies)       -> PolicyList{ items, next_cursor? }
//   - GET /policies/{policyId}             (getPolicy)          -> Policy
//   - GET /policies/{policyId}/versions    (listPolicyVersions) -> PolicyVersionList{ items }
//
// All send Authorization: Bearer <token> using the token stored by `login`
// (loaded via internal/clitoken). Non-2xx responses carry the flat Error
// envelope (code + message), surfaced to the operator via the shared helpers
// in agents.go (loadToken, getJSON, apiErrorFromResponse, decodeAPIError).
//
// Read-only by design: create/publish (POST /policies, POST .../versions) are
// deliberately not exposed here.

// policy mirrors the Policy schema in openapi/v1.yaml.
type policy struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	CurrentVersion int    `json:"current_version"`
	CreatedAt      string `json:"created_at"`
}

// policyList mirrors the PolicyList schema: a page of policies plus the Page
// envelope's optional next_cursor.
type policyList struct {
	Items      []policy `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// signingMetadata mirrors the SigningMetadata schema: detached-signature
// metadata proving policy provenance.
type signingMetadata struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
	SignedAt  string `json:"signed_at,omitempty"`
}

// policyVersion mirrors the PolicyVersion schema: an immutable, signed policy
// version. The boundary document is free-form at this contract stage, so it is
// carried as raw JSON rather than a typed struct.
type policyVersion struct {
	PolicyID  string          `json:"policy_id"`
	Version   int             `json:"version"`
	Digest    string          `json:"digest"`
	Boundary  json.RawMessage `json:"boundary,omitempty"`
	Signing   signingMetadata `json:"signing"`
	CreatedAt string          `json:"created_at"`
}

// policyVersionList mirrors the PolicyVersionList schema: the (unpaginated)
// list of a policy's versions.
type policyVersionList struct {
	Items []policyVersion `json:"items"`
}

// policiesCmd dispatches the `policies` subcommands (list, get, versions).
func policiesCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		policiesUsage(stderr)
		return 2
	}
	switch sub := args[0]; sub {
	case "list":
		return policiesListCmd(args[1:], stdout, stderr)
	case "get":
		return policiesGetCmd(args[1:], stdout, stderr)
	case "versions":
		return policiesVersionsCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		policiesUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "drun-dash policies: unknown subcommand %q\n\n", sub)
		policiesUsage(stderr)
		return 2
	}
}

// policiesUsage writes help for the `policies` command tree.
func policiesUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: drun-dash policies <subcommand> [flags]

Read-only access to the organization's policy boundaries and their signed
versions.

Subcommands:
  list                    List policies (GET /policies)
  get <policyId>          Show a single policy's detail (GET /policies/{policyId})
  versions <policyId>     List a policy's signed versions (GET /policies/{policyId}/versions)

Run "drun-dash policies <subcommand> -h" for subcommand-specific flags.
`)
}

// policiesListCmd calls GET /policies and prints a readable table, passing
// --limit / --cursor through as query params and hinting when more pages exist.
func policiesListCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policies list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	limit := fs.Int("limit", 0, "maximum number of policies to return (server default when unset)")
	cursor := fs.String("cursor", "", "pagination cursor from a previous page's next_cursor")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash policies list [flags]

List policy boundaries in the organization via GET /policies, using the stored
session token.

Flags:
  --limit N      maximum number of policies to return (server default when unset)
  --cursor C     pagination cursor from a previous page's next_cursor
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tok, ok := loadToken("policies list", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	list, err := fetchPolicyList(hc, base, tok.AccessToken, *limit, *cursor)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash policies list: %v\n", err)
		return 1
	}

	printPolicyList(stdout, list)
	return 0
}

// policiesGetCmd calls GET /policies/{policyId} and prints the policy detail. A
// 404 prints a clear not-found message and exits non-zero.
func policiesGetCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policies get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash policies get <policyId> [flags]

Fetch and print a single policy via GET /policies/{policyId}, using the stored
session token.

Flags:
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "drun-dash policies get: exactly one <policyId> argument is required")
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	tok, ok := loadToken("policies get", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	p, err := fetchPolicy(hc, base, tok.AccessToken, id)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash policies get: %v\n", err)
		return 1
	}

	printPolicyDetail(stdout, p)
	return 0
}

// policiesVersionsCmd calls GET /policies/{policyId}/versions and lists the
// policy's signed versions with their digest. A 404 prints a clear not-found
// message and exits non-zero.
func policiesVersionsCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policies versions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash policies versions <policyId> [flags]

List a policy's immutable, signed versions via GET /policies/{policyId}/versions,
using the stored session token. Each version prints its digest and signing
algorithm.

Flags:
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "drun-dash policies versions: exactly one <policyId> argument is required")
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	tok, ok := loadToken("policies versions", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	list, err := fetchPolicyVersions(hc, base, tok.AccessToken, id)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash policies versions: %v\n", err)
		return 1
	}

	printPolicyVersionList(stdout, id, list)
	return 0
}

// fetchPolicyList performs GET /policies with the Bearer token, passing limit
// and cursor as query params when set.
func fetchPolicyList(hc *http.Client, base, token string, limit int, cursor string) (*policyList, error) {
	u := base + "/policies"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
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
	var list policyList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode policy list: %w", err)
	}
	return &list, nil
}

// fetchPolicy performs GET /policies/{policyId} with the Bearer token. A 404 is
// reported as a clear not-found error.
func fetchPolicy(hc *http.Client, base, token, id string) (*policy, error) {
	body, status, err := getJSON(hc, base+"/policies/"+url.PathEscape(id), token)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var p policy
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("decode policy: %w", err)
		}
		return &p, nil
	case http.StatusNotFound:
		if e, ok := decodeAPIError(body); ok && e.Message != "" {
			return nil, fmt.Errorf("policy %q not found: %s", id, e.Message)
		}
		return nil, fmt.Errorf("policy %q not found", id)
	default:
		return nil, apiErrorFromResponse(status, body)
	}
}

// fetchPolicyVersions performs GET /policies/{policyId}/versions with the Bearer
// token. A 404 (unknown policy) is reported as a clear not-found error.
func fetchPolicyVersions(hc *http.Client, base, token, id string) (*policyVersionList, error) {
	body, status, err := getJSON(hc, base+"/policies/"+url.PathEscape(id)+"/versions", token)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var list policyVersionList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode policy versions: %w", err)
		}
		return &list, nil
	case http.StatusNotFound:
		if e, ok := decodeAPIError(body); ok && e.Message != "" {
			return nil, fmt.Errorf("policy %q not found: %s", id, e.Message)
		}
		return nil, fmt.Errorf("policy %q not found", id)
	default:
		return nil, apiErrorFromResponse(status, body)
	}
}

// printPolicyList renders the policies as an aligned table and hints when
// another page is available.
func printPolicyList(w io.Writer, list *policyList) {
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "No policies found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCURRENT\tDESCRIPTION")
	for _, p := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", p.ID, p.Name, p.CurrentVersion, p.Description)
	}
	tw.Flush()
	if list.NextCursor != "" {
		fmt.Fprintf(w, "\nMore available — re-run with --cursor %s\n", list.NextCursor)
	}
}

// printPolicyDetail renders a single policy's fields, omitting empty optionals.
func printPolicyDetail(w io.Writer, p *policy) {
	fmt.Fprintf(w, "ID:           %s\n", p.ID)
	fmt.Fprintf(w, "Name:         %s\n", p.Name)
	if p.Description != "" {
		fmt.Fprintf(w, "Description:  %s\n", p.Description)
	}
	fmt.Fprintf(w, "Current ver:  %d\n", p.CurrentVersion)
	fmt.Fprintf(w, "Created:      %s\n", p.CreatedAt)
}

// printPolicyVersionList renders a policy's versions as an aligned table, one
// row per version with its digest and signing algorithm.
func printPolicyVersionList(w io.Writer, id string, list *policyVersionList) {
	if len(list.Items) == 0 {
		fmt.Fprintf(w, "No versions found for policy %s.\n", id)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tDIGEST\tALGORITHM\tKEY\tCREATED")
	for _, v := range list.Items {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", v.Version, v.Digest, v.Signing.Algorithm, v.Signing.KeyID, v.CreatedAt)
	}
	tw.Flush()
}
