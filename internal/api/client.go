// Package api is the JumpServer REST transport.
//
// It owns the shared *http.Client (cookie jar included), JSON parsing,
// error classification, the retry policy and limit/offset pagination.
// Login orchestration lives in internal/auth; this package only moves
// bytes.
//
// Error classification mirrors the Python reference implementation:
// HTTP 401 raises AuthError (bad or expired token), any other non-2xx
// raises APIError, and a transport failure raises APIError with
// StatusCode == 0.
//
// Dependency direction: api depends on nothing else in this module.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout is the per-request HTTP timeout (DESIGN.md §4.5).
const DefaultTimeout = 15 * time.Second

// DefaultPageSize is the page size used by GetAll.
const DefaultPageSize = 100

// maxErrorBody bounds how much of a response body is embedded in an
// error message, so a large error page cannot flood the terminal.
const maxErrorBody = 200

// Retry tuning: 3 retries with 0.5s/1s/2s backoff on transport errors
// and on 502/503/504 (DESIGN.md §4.5). Every method is retried, POST
// included, matching the Python client's urllib3 policy.
const (
	maxRetries  = 3
	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 2 * time.Second
)

// maxPages bounds GetAll. Offset advances by at least one item per page,
// so a server that reports a huge (or absent) count and ignores offset
// would otherwise keep the client looping indefinitely.
const maxPages = 1000

// sleepFn is indirected so tests can observe the backoff schedule
// without waiting for it.
var sleepFn = sleepWithContext

func sleepWithContext(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// retryableStatus reports whether a response status is worth retrying.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// Client is an HTTP client bound to one JumpServer base URL.
//
// It is safe for concurrent use once login has finished: the embedded
// *http.Client and its cookie jar are, and the token fields are only
// written while the session is still private to the login flow.
type Client struct {
	baseURL string
	http    *http.Client

	bearer string
	csrf   string
}

// New returns a Client for baseURL. The URL is normalized: whitespace
// trimmed, trailing slashes removed and https:// assumed when the scheme
// is missing. The client gets a cookie jar because the Django form login
// only communicates through cookies.
func New(baseURL string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL: normalizeBaseURL(baseURL),
		http: &http.Client{
			Timeout: DefaultTimeout,
			Jar:     jar,
		},
	}
}

// normalizeBaseURL trims whitespace, strips trailing slashes and adds
// https:// when the scheme is missing.
func normalizeBaseURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return strings.TrimRight(u, "/")
}

// BaseURL returns the normalized base URL.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// HTTP exposes the underlying client (used by internal/auth for the form
// login and by tests to swap transports).
func (c *Client) HTTP() *http.Client {
	if c == nil {
		return nil
	}
	return c.http
}

// SetHTTPClient replaces the underlying HTTP client. Intended for tests
// and for wiring a client with custom TLS settings.
func (c *Client) SetHTTPClient(hc *http.Client) {
	if c == nil || hc == nil {
		return
	}
	c.http = hc
}

// SetBearer stores the API bearer token sent as "Authorization: Bearer".
func (c *Client) SetBearer(token string) {
	if c == nil {
		return
	}
	c.bearer = token
}

// Bearer returns the current bearer token.
func (c *Client) Bearer() string {
	if c == nil {
		return ""
	}
	return c.bearer
}

// SetCSRF stores the CSRF token sent as "X-CSRFToken".
func (c *Client) SetCSRF(token string) {
	if c == nil {
		return
	}
	c.csrf = token
}

// CSRF returns the current CSRF token.
func (c *Client) CSRF() string {
	if c == nil {
		return ""
	}
	return c.csrf
}

// Cookie returns the value of the named cookie in the client's jar for
// the base URL, or "" when absent.
func (c *Client) Cookie(name string) string {
	if c == nil || c.http == nil || c.http.Jar == nil {
		return ""
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// Get performs an authenticated GET and decodes the JSON body into out.
// out may be nil when the body is not needed.
func (c *Client) Get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, params, nil, out)
}

// Post performs an authenticated POST with a JSON body and decodes the
// JSON response into out. out may be nil.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// GetAll walks every page of a limit/offset paginated list endpoint and
// returns the raw items.
//
// JumpServer answers {"count": N, "results": [...]}; a bare JSON array
// (pagination disabled server-side) is returned as-is. Iteration stops on
// an empty page, when the accumulated offset reaches count, or after
// maxPages pages — the last bound keeps a server that ignores offset
// from looping the client forever.
func (c *Client) GetAll(ctx context.Context, path string, params url.Values) ([]json.RawMessage, error) {
	var items []json.RawMessage
	offset := 0

	for page := 0; page < maxPages; page++ {
		// A fresh map per page: the server may retain the query.
		pageParams := url.Values{}
		for k, vs := range params {
			pageParams[k] = append([]string(nil), vs...)
		}
		pageParams.Set("limit", strconv.Itoa(DefaultPageSize))
		pageParams.Set("offset", strconv.Itoa(offset))

		var raw json.RawMessage
		if err := c.Get(ctx, path, pageParams, &raw); err != nil {
			return nil, err
		}

		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" {
			return items, nil
		}

		switch trimmed[0] {
		case '[':
			var bare []json.RawMessage
			if err := json.Unmarshal(raw, &bare); err != nil {
				return nil, &APIError{StatusCode: 0, Body: fmt.Sprintf(
					"API GET %s: invalid list body: %v", path, err)}
			}
			return append(items, bare...), nil

		case '{':
			var body struct {
				Count   *int              `json:"count"`
				Results []json.RawMessage `json:"results"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				return nil, &APIError{StatusCode: 0, Body: fmt.Sprintf(
					"API GET %s: invalid page body: %v", path, err)}
			}
			if len(body.Results) == 0 {
				return items, nil
			}
			items = append(items, body.Results...)
			offset += len(body.Results)
			if body.Count != nil && offset >= *body.Count {
				return items, nil
			}

		default:
			return nil, &APIError{StatusCode: 0, Body: fmt.Sprintf(
				"API GET %s: unexpected JSON body: %s", path, truncateBody(raw))}
		}
	}

	return nil, &APIError{StatusCode: 0, Body: fmt.Sprintf(
		"API GET %s: pagination did not terminate after %d pages", path, maxPages)}
}

// do sends one request, retrying transport failures and 502/503/504 up
// to maxRetries times.
func (c *Client) do(ctx context.Context, method, path string, params url.Values, body, out any) error {
	if c == nil {
		return &APIError{StatusCode: 0, Body: fmt.Sprintf("API %s %s: nil client", method, path)}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	endpoint := c.baseURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return &APIError{StatusCode: 0, Body: fmt.Sprintf(
				"API %s %s: encode body: %v", method, path, err)}
		}
		payload = encoded
	}

	var lastErr error
	max := maxRetries
	if budget := budgetFrom(ctx); budget >= 0 {
		max = budget
	}
	for attempt := 0; attempt <= max; attempt++ {
		// A cancelled or expired context must not burn the retry
		// budget, so it is checked before every attempt.
		if err := ctx.Err(); err != nil {
			return &APIError{StatusCode: 0, Body: fmt.Sprintf("API %s %s: %v", method, path, err)}
		}
		if attempt > 0 {
			sleepFn(ctx, backoffFor(attempt))
		}

		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return &APIError{StatusCode: 0, Body: fmt.Sprintf(
				"API %s %s: build request: %v", method, path, err)}
		}
		req.Header.Set("Content-Type", "application/json")
		if c.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		if c.csrf != "" {
			req.Header.Set("X-CSRFToken", c.csrf)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return &APIError{StatusCode: 0, Body: fmt.Sprintf(
					"API %s %s: %v", method, path, ctxErr)}
			}
			lastErr = &APIError{StatusCode: 0, Body: fmt.Sprintf("API %s %s: %v", method, path, err)}
			continue
		}

		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = &APIError{StatusCode: 0, Body: fmt.Sprintf(
				"API %s %s: read body: %v", method, path, readErr)}
			continue
		}

		if retryableStatus(resp.StatusCode) {
			lastErr = apiFailure(method, path, resp.StatusCode, raw)
			continue
		}
		return decodeResponse(method, path, resp.StatusCode, raw, out)
	}
	return lastErr
}

// apiFailure builds the APIError for a non-2xx response other than 401.
func apiFailure(method, path string, status int, raw []byte) *APIError {
	return &APIError{StatusCode: status, Body: fmt.Sprintf("API %s %s failed (HTTP %d): %s",
		method, path, status, truncateBody(raw))}
}

// decodeResponse applies the error classification to a response.
func decodeResponse(method, path string, status int, raw []byte, out any) error {
	switch {
	case status == http.StatusUnauthorized:
		return &AuthError{StatusCode: status, Body: fmt.Sprintf(
			"API %s %s unauthorized (HTTP 401): %s", method, path, truncateBody(raw))}
	case status < 200 || status >= 300:
		return apiFailure(method, path, status, raw)
	}

	// 204 and an empty body carry no JSON: leave out untouched so a
	// caller's pre-filled value survives.
	if status == http.StatusNoContent || strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &APIError{StatusCode: status, Body: fmt.Sprintf(
			"API %s %s: invalid JSON response (HTTP %d): %s",
			method, path, status, truncateBody(raw))}
	}
	return nil
}

// backoffFor returns the pause before the given attempt (1-based):
// 0.5s, 1s, 2s.
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	d := baseBackoff
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// truncateBody bounds an error body. It never includes request headers,
// so credentials cannot leak through an error message.
func truncateBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > maxErrorBody {
		return s[:maxErrorBody]
	}
	return s
}
