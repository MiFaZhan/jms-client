package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// serveAssets starts an httptest server, wraps it in an api.Client and
// registers cleanup. No real network is contacted.
func serveAssets(t *testing.T, handler http.HandlerFunc) *api.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return api.New(srv.URL)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func page(results ...map[string]any) map[string]any {
	return map[string]any{"count": len(results), "results": results}
}

// listingHandler serves the asset listing, paging through results by the
// offset query parameter (api.Client.GetAll always sends offset/limit).
func listingHandler(t *testing.T, results []map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &offset); err != nil {
				t.Errorf("bad offset %q: %v", v, err)
			}
		}
		if offset >= len(results) {
			writeJSON(t, w, map[string]any{"count": len(results), "results": []any{}})
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &limit); err != nil {
				t.Errorf("bad limit %q: %v", v, err)
			}
		}
		end := offset + limit
		if end > len(results) {
			end = len(results)
		}
		writeJSON(t, w, map[string]any{"count": len(results), "results": results[offset:end]})
	}
}

func TestListPaginatesAndRespectsLimit(t *testing.T) {
	results := []map[string]any{
		{"id": "1", "name": "a", "address": "10.0.0.1"},
		{"id": "2", "name": "b", "address": "10.0.0.2"},
		{"id": "3", "name": "c", "address": "10.0.0.3"},
	}
	var searches []string
	client := serveAssets(t, func(w http.ResponseWriter, r *http.Request) {
		searches = append(searches, r.URL.Query().Get("search"))
		listingHandler(t, results)(w, r)
	})

	all, err := List(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("List(0): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(0) returned %d assets, want 3", len(all))
	}
	if all[0].Name != "a" || all[2].ID != "3" {
		t.Fatalf("List(0) decoded wrong rows: %+v", all)
	}

	limited, err := List(context.Background(), client, 2)
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("List(2) returned %d assets, want 2", len(limited))
	}
	if limited[1].Name != "b" {
		t.Fatalf("List(2) kept wrong prefix: %+v", limited)
	}

	negative, err := List(context.Background(), client, -1)
	if err != nil {
		t.Fatalf("List(-1): %v", err)
	}
	if len(negative) != 3 {
		t.Fatalf("List(-1) returned %d assets, want all 3", len(negative))
	}

	for _, s := range searches {
		if s != "" {
			t.Fatalf("List sent a search parameter %q; it must not", s)
		}
	}
}

func TestListSpansAllPages(t *testing.T) {
	// GetAll pages at 100 items, so >100 rows force a second request.
	results := make([]map[string]any, 150)
	for i := range results {
		results[i] = map[string]any{
			"id":      fmt.Sprintf("%d", i),
			"name":    fmt.Sprintf("server-%03d", i),
			"address": "10.0.0.1",
		}
	}
	var offsets []string
	client := serveAssets(t, func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		listingHandler(t, results)(w, r)
	})

	all, err := List(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 150 {
		t.Fatalf("List returned %d assets, want all 150 across both pages", len(all))
	}
	if all[0].ID != "0" || all[149].ID != "149" {
		t.Fatalf("List lost rows across pages: first=%q last=%q", all[0].ID, all[149].ID)
	}
	if len(offsets) < 2 {
		t.Fatalf("only %d page request(s) made: %v", len(offsets), offsets)
	}

	limited, err := List(context.Background(), client, 5)
	if err != nil {
		t.Fatalf("List(5): %v", err)
	}
	if len(limited) != 5 {
		t.Fatalf("List(5) returned %d assets, want 5", len(limited))
	}
}

func TestListAndSearchDecodeBothPlatformShapes(t *testing.T) {
	results := []map[string]any{
		{"id": "1", "name": "obj", "address": "10.0.0.1", "platform": map[string]any{"name": "Linux"}},
		{"id": "2", "name": "str", "address": "10.0.0.2", "platform": "Windows"},
	}
	client := serveAssets(t, listingHandler(t, results))

	got, err := List(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got[0].Platform != "Linux" {
		t.Fatalf("object platform decoded as %q, want Linux", got[0].Platform)
	}
	if got[1].Platform != "Windows" {
		t.Fatalf("string platform decoded as %q, want Windows", got[1].Platform)
	}

	searched, err := Search(context.Background(), client, "x")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if searched[0].Platform != "Linux" || searched[1].Platform != "Windows" {
		t.Fatalf("Search decoded platforms as %q, %q", searched[0].Platform, searched[1].Platform)
	}
}

// TestListDecodesTypeLabel pins the Type column the listing renders. The
// API returns type as {"label":...,"value":...} on current versions and as
// a bare string on older ones.
func TestListDecodesTypeLabel(t *testing.T) {
	results := []map[string]any{
		{"id": "1", "name": "a", "type": map[string]any{"label": "网站", "value": "website"}},
		{"id": "2", "name": "b", "type": map[string]any{"value": "linux"}},
		{"id": "3", "name": "c", "type": "Windows"},
		{"id": "4", "name": "d"},
		{"id": "5", "name": "e", "type": nil},
	}
	client := serveAssets(t, listingHandler(t, results))

	got, err := List(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"网站", "linux", "Windows", "", ""}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Type != w {
			t.Errorf("row %d Type = %q, want %q", i, got[i].Type, w)
		}
	}
}

func TestSearchSendsSearchParameter(t *testing.T) {
	var got string
	client := serveAssets(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("search")
		listingHandler(t, []map[string]any{{"id": "1", "name": "db"}})(w, r)
	})

	if _, err := Search(context.Background(), client, "db server"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got != "db server" {
		t.Fatalf("search parameter = %q, want %q", got, "db server")
	}
}

func TestSearchEmptyResult(t *testing.T) {
	client := serveAssets(t, listingHandler(t, nil))
	got, err := Search(context.Background(), client, "nope")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Search returned %d assets, want 0", len(got))
	}
}

// detailJSON is the shared asset detail payload used by the Resolve tests.
func detailJSON() map[string]any {
	return map[string]any{
		"permed_accounts": []map[string]any{
			{"alias": "@USER", "username": "dynamic", "name": "Dynamic user"},
			{"alias": "gcszhn", "username": "gcszhn", "name": "gcszhn"},
		},
		"permed_protocols": []map[string]any{
			{"name": "ssh", "port": 22},
			{"name": "sftp", "port": 22},
		},
	}
}

// resolveClient serves the listing and the per-id detail endpoint.
func resolveClient(t *testing.T, results []map[string]any, details map[string]map[string]any) *api.Client {
	t.Helper()
	listing := listingHandler(t, results)
	return serveAssets(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathSelfAssets {
			listing(w, r)
			return
		}
		id := r.URL.Path[len(PathSelfAssets):]
		id = id[:len(id)-1] // strip trailing "/"
		detail, ok := details[id]
		if !ok {
			http.Error(w, "no such asset", http.StatusNotFound)
			return
		}
		writeJSON(t, w, detail)
	})
}

func TestResolvePrefersExactNameMatch(t *testing.T) {
	results := []map[string]any{
		{"id": "2", "name": "db_server", "address": "10.0.0.2"},
		{"id": "1", "name": "home_server", "address": "192.168.1.20",
			"platform": map[string]any{"name": "Linux"}, "org_id": "org-1"},
	}
	client := resolveClient(t, results, map[string]map[string]any{"1": detailJSON()})

	info, err := Resolve(context.Background(), client, "home_server", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.ID != "1" {
		t.Fatalf("Resolve picked id %q, want the exact match 1", info.ID)
	}
	if info.Name != "home_server" {
		t.Fatalf("Name = %q, want home_server", info.Name)
	}
	if info.Address != "192.168.1.20" {
		t.Fatalf("Address = %q", info.Address)
	}
	if info.Account != "@USER" {
		t.Fatalf("Account = %q, want @USER", info.Account)
	}
	if info.Protocol != "ssh" {
		t.Fatalf("Protocol = %q, want ssh", info.Protocol)
	}
	if info.Platform != "Linux" {
		t.Fatalf("Platform = %q, want Linux", info.Platform)
	}
	if info.OrgID != "org-1" {
		t.Fatalf("OrgID = %q, want org-1", info.OrgID)
	}
}

// TestResolveExactMatchOnLaterPage is the Go port of the Python
// regression: an exact match beyond the first page must still win.
func TestResolveExactMatchOnLaterPage(t *testing.T) {
	results := []map[string]any{
		{"id": "2", "name": "server-aa", "address": "10.0.0.2"},
		{"id": "1", "name": "home_server", "address": "192.168.1.20"},
	}
	client := resolveClient(t, results, map[string]map[string]any{"1": detailJSON()})

	info, err := Resolve(context.Background(), client, "home_server", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.ID != "1" || info.Name != "home_server" {
		t.Fatalf("Resolve = %+v, want the second-page exact match", info)
	}
}

func TestResolveFallsBackToFirstResult(t *testing.T) {
	results := []map[string]any{
		{"id": "2", "name": "db_server", "address": "10.0.0.2"},
	}
	client := resolveClient(t, results, map[string]map[string]any{"2": detailJSON()})

	info, err := Resolve(context.Background(), client, "db", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.ID != "2" {
		t.Fatalf("Resolve picked id %q, want the first result 2", info.ID)
	}
	if info.Name != "db_server" {
		t.Fatalf("Name = %q, want db_server", info.Name)
	}
}

func TestResolveNotFound(t *testing.T) {
	client := serveAssets(t, listingHandler(t, nil))

	_, err := Resolve(context.Background(), client, "ghost", "", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve error = %v, want ErrNotFound", err)
	}
}

func TestResolveHonoursOverrides(t *testing.T) {
	results := []map[string]any{
		{"id": "1", "name": "home_server", "address": "192.168.1.20"},
	}
	client := resolveClient(t, results, map[string]map[string]any{"1": detailJSON()})

	info, err := Resolve(context.Background(), client, "home_server", "gcszhn", "sftp")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.Account != "gcszhn" {
		t.Fatalf("Account = %q, want the override gcszhn", info.Account)
	}
	if info.Protocol != "sftp" {
		t.Fatalf("Protocol = %q, want the override sftp", info.Protocol)
	}
}

func TestResolveSkipsMalformedRows(t *testing.T) {
	// The first row is malformed; the exact match is the second.
	client := serveAssets(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathSelfAssets {
			writeJSON(t, w, map[string]any{
				"count": 2,
				"results": []any{
					"not-an-object",
					map[string]any{"id": "1", "name": "home_server", "address": "192.168.1.20"},
				},
			})
			return
		}
		writeJSON(t, w, detailJSON())
	})

	info, err := Resolve(context.Background(), client, "home_server", "", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if info.ID != "1" {
		t.Fatalf("Resolve picked id %q, want 1 after skipping the malformed row", info.ID)
	}
}

func TestListSkipsMalformedRows(t *testing.T) {
	client := serveAssets(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"count": 4,
			"results": []any{
				map[string]any{"id": "1", "name": "good"},
				123,
				"garbage",
				map[string]any{"id": 7, "name": "numeric-id"},
				map[string]any{"id": "2", "name": "also-good"},
			},
		})
	})

	got, err := List(context.Background(), client, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d assets, want the 2 well-formed rows: %+v", len(got), got)
	}
	if got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("List kept the wrong rows: %+v", got)
	}
}

func TestSelectAccountMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   []map[string]any
		want string
	}{
		{
			name: "user alias wins over named account",
			in: []map[string]any{
				{"alias": "gcszhn", "username": "gcszhn"},
				{"alias": "@USER", "username": "dynamic"},
			},
			want: "@USER",
		},
		{
			name: "named account wins over input",
			in: []map[string]any{
				{"alias": "@INPUT", "username": ""},
				{"alias": "gcszhn", "username": "gcszhn"},
			},
			want: "gcszhn",
		},
		{
			name: "username used when alias is at-prefixed",
			in: []map[string]any{
				{"alias": "@ANON", "username": "bob"},
			},
			want: "bob",
		},
		{
			name: "username used when alias is empty",
			in: []map[string]any{
				{"alias": "", "username": "bob"},
			},
			want: "bob",
		},
		{
			name: "at-prefixed entries skipped, first entry alias used",
			in: []map[string]any{
				{"alias": "@ANON", "username": ""},
			},
			want: "@ANON",
		},
		{
			name: "first entry username used when alias absent",
			in: []map[string]any{
				{"username": "bob"},
			},
			want: "bob",
		},
		{
			name: "empty entry falls back to input",
			in:   []map[string]any{{}},
			want: "@INPUT",
		},
		{
			name: "no entries falls back to input",
			in:   nil,
			want: "@INPUT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectAccount(tc.in); got != tc.want {
				t.Fatalf("SelectAccount(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSelectProtocolMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   []map[string]any
		want string
	}{
		{
			name: "ssh preferred even when listed second",
			in:   []map[string]any{{"name": "rdp"}, {"name": "ssh"}},
			want: "ssh",
		},
		{
			name: "ssh match is case insensitive and canonicalised",
			in:   []map[string]any{{"name": "rdp"}, {"name": "SSH"}},
			want: "ssh",
		},
		{
			name: "no ssh falls back to the first entry",
			in:   []map[string]any{{"name": "rdp"}, {"name": "telnet"}},
			want: "rdp",
		},
		{
			name: "empty falls back to ssh",
			in:   nil,
			want: "ssh",
		},
		{
			name: "first entry without a name falls back to ssh",
			in:   []map[string]any{{}},
			want: "ssh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectProtocol(tc.in); got != tc.want {
				t.Fatalf("SelectProtocol(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
