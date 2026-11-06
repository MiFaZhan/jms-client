package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// List paging bounds.
//
// Every management collection on the bastion is large: the instance this was
// verified against held 166,995 login records, 6,314 terminal sessions and
// 3,620 operation records on the day it was measured. A tool that returns
// "everything" would therefore return a page no caller can read, so a limit
// is not a convenience here — it is the difference between a usable tool and
// a timeout.
const (
	// DefaultLimit applies when the caller passes no limit.
	DefaultLimit = 20
	// MaxLimit is the largest page any read tool will request.
	MaxLimit = 100
)

// ListOptions carries paging plus the filters that are applied by the
// server.
type ListOptions struct {
	// Limit is the page size; <= 0 means DefaultLimit.
	Limit int
	// Offset skips that many records.
	Offset int
	// Filters holds query parameters sent to the server. Only parameters
	// whose meaning was verified against a live instance belong here.
	Filters url.Values
}

// ClampLimit bounds a caller's page size.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// values renders the query for one request.
func (o ListOptions) values() url.Values {
	v := url.Values{}
	for k, vals := range o.Filters {
		for _, item := range vals {
			v.Add(k, item)
		}
	}
	v.Set("limit", strconv.Itoa(ClampLimit(o.Limit)))
	if o.Offset > 0 {
		v.Set("offset", strconv.Itoa(o.Offset))
	}
	return v
}

// FlexString decodes a field whose shape depends on the build and on the
// record: a plain string, a {"value","label"} object, a boolean or a number.
//
// This is not defensive over-engineering — it is what the live instance
// returns. On the reference JumpServer v4 a login record carries
// `"status":{"value":true,"label":"成功"}` and `"type":{"value":"W","label":"Web"}`,
// while an account carries `"secret_type":{"value":"password",...}`. A plain
// string field would fail the whole record, turning "one column has an
// unexpected shape" into "the audit tool is broken".
//
// Resolution order: a non-empty string value, then the label, then the
// stringified value. For a boolean-valued field that yields the label
// ("成功") rather than "true", which is what a reader wants.
type FlexString string

// UnmarshalJSON accepts a string, number, boolean, null, or {"value","label"}.
func (f *FlexString) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = FlexString(s)
		return nil
	}

	var obj struct {
		Value any    `json:"value"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		if value, ok := obj.Value.(string); ok && value != "" {
			*f = FlexString(value)
			return nil
		}
		if obj.Label != "" {
			*f = FlexString(obj.Label)
			return nil
		}
		if obj.Value != nil {
			*f = FlexString(fmt.Sprint(obj.Value))
			return nil
		}
		*f = ""
		return nil
	}

	// A bare number or boolean.
	var anyScalar any
	if err := json.Unmarshal(data, &anyScalar); err == nil && anyScalar != nil {
		*f = FlexString(fmt.Sprint(anyScalar))
		return nil
	}
	*f = ""
	return nil
}

// Page is one slice of a JumpServer collection.
//
// Count is the server's total, which is not the length of Results: callers
// report both so a reader can tell "there are 2 of these" from "here are 2,
// there are 166,995".
type Page[T any] struct {
	Count   int `json:"count"`
	Results []T `json:"results"`
}

// listPage fetches one page of a collection.
func listPage[T any](ctx context.Context, c *api.Client, path string, opts ListOptions) (Page[T], error) {
	var envelope struct {
		Count   int `json:"count"`
		Results []T `json:"results"`
	}
	if err := c.Get(ctx, path, opts.values(), &envelope); err != nil {
		return Page[T]{}, err
	}
	if envelope.Results == nil {
		envelope.Results = []T{}
	}
	return Page[T]{Count: envelope.Count, Results: envelope.Results}, nil
}

// DateRange adds the two verified server-side time filters.
//
// `date_from` and `date_to` are the only filters whose server-side meaning
// was confirmed on a live instance (a login-log query with date_from=today
// narrowed 166,995 records to 109, and the session list from 6,314 to 2).
// Every other filter a tool offers is applied to the page it fetched, and
// the tool says so rather than pretending the filter was global.
func DateRange(filters url.Values, since, until string) url.Values {
	if filters == nil {
		filters = url.Values{}
	}
	if s := strings.TrimSpace(since); s != "" {
		filters.Set("date_from", s)
	}
	if s := strings.TrimSpace(until); s != "" {
		filters.Set("date_to", s)
	}
	return filters
}

// Window is how many records to request when a caller passed a filter that
// can only be applied locally.
//
// Scanning the maximum page is deliberate: filtering 20 records by user and
// reporting "none" would be a false answer, while scanning 100 and saying
// "scanned the 100 most recent" is a true one.
func Window(limit int, hasLocalFilters bool) int {
	if !hasLocalFilters {
		return ClampLimit(limit)
	}
	return MaxLimit
}

// MatchKeyword reports whether any candidate contains the keyword,
// case-insensitively. An empty keyword matches everything.
func MatchKeyword(keyword string, candidates ...string) bool {
	k := strings.ToLower(strings.TrimSpace(keyword))
	if k == "" {
		return true
	}
	for _, c := range candidates {
		if strings.Contains(strings.ToLower(c), k) {
			return true
		}
	}
	return false
}

// FilterRows applies a caller-supplied predicate to one page and trims it
// to the requested size.
//
// It returns the kept rows and whether the scan hit the window limit, which
// the caller reports as "scanned the N most recent" so a filtered answer is
// never mistaken for a global one.
func FilterRows[T any](rows []T, limit int, keep func(T) bool) ([]T, bool) {
	if keep == nil {
		return trimRows(rows, limit), false
	}
	kept := make([]T, 0, len(rows))
	for _, row := range rows {
		if keep(row) {
			kept = append(kept, row)
		}
	}
	return trimRows(kept, limit), true
}

// trimRows bounds rows to limit.
func trimRows[T any](rows []T, limit int) []T {
	n := ClampLimit(limit)
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}
