package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Pagination bounds from the OpenAPI contract's PageLimit parameter
// (openapi/v1.yaml: minimum 1, maximum 200, default 50). ParseLimit enforces
// them per ADR-0002 §3.
const (
	// DefaultLimit is the page size used when the request omits ?limit.
	DefaultLimit = 50
	// MinLimit is the smallest page size; smaller values are clamped up.
	MinLimit = 1
	// MaxLimit is the largest page size; larger values are clamped down.
	MaxLimit = 200
)

// cursorSep separates the RFC3339Nano timestamp from the id inside the decoded
// cursor payload. A NUL byte can never appear in either half (RFC3339Nano is
// ASCII; ids are UUID strings), so it is an unambiguous delimiter.
const cursorSep = "\x00"

// ErrInvalidCursor is returned by DecodeCursor when the cursor parameter is
// present but does not base64url-decode to a valid "<RFC3339Nano>\x00<id>"
// payload. Handlers map it to 400 bad_request ("invalid cursor"); it must never
// be treated as an empty cursor (a silent reset to the first page would loop
// clients forever).
var ErrInvalidCursor = errors.New("httpapi: invalid cursor")

// ErrInvalidLimit is returned by ParseLimit when ?limit is present but not a
// base-10 integer. Handlers map it to 400 bad_request. An out-of-range integer
// is not an error — it is clamped to [MinLimit, MaxLimit].
var ErrInvalidLimit = errors.New("httpapi: invalid limit")

// Cursor is the decoded keyset position: the (created_at, id) tuple of the last
// row returned on the previous page. The store uses it as the seek predicate
// WHERE (created_at, id) > ($CreatedAt, $ID) over ORDER BY created_at, id
// (ADR-0002 §3). It carries no org id — the org is always re-derived from the
// token — so a cursor cannot be replayed to page another tenant.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ParseLimit reads the ?limit query parameter and returns the effective page
// size per ADR-0002 §3:
//
//   - absent            → DefaultLimit (50)
//   - non-integer       → ErrInvalidLimit (handler maps to 400 bad_request)
//   - integer < MinLimit → clamped up to MinLimit (1)
//   - integer > MaxLimit → clamped down to MaxLimit (200)
//
// Out-of-range integers are clamped rather than rejected, so a client asking
// for limit=1000 gets 200 instead of an error.
func ParseLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, ErrInvalidLimit
	}
	if n < MinLimit {
		return MinLimit, nil
	}
	if n > MaxLimit {
		return MaxLimit, nil
	}
	return n, nil
}

// EncodeCursor serializes a Cursor to the opaque next_cursor string:
// base64url(created_at RFC3339Nano + "\x00" + id), with no padding (ADR-0002
// §3). The result is opaque to clients — they echo it back verbatim via the
// cursor parameter.
func EncodeCursor(c Cursor) string {
	payload := c.CreatedAt.UTC().Format(time.RFC3339Nano) + cursorSep + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// DecodeCursor parses an opaque cursor string back into a Cursor.
//
// An empty string means "start from the beginning" and returns (nil, nil). A
// non-empty string that fails to base64url-decode, or whose decoded form is not
// a valid "<RFC3339Nano>\x00<id>" pair, returns ErrInvalidCursor; handlers map
// that to 400 bad_request rather than silently restarting pagination.
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	tsStr, id, found := strings.Cut(string(raw), cursorSep)
	if !found || id == "" {
		return nil, ErrInvalidCursor
	}
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	return &Cursor{CreatedAt: ts, ID: id}, nil
}
