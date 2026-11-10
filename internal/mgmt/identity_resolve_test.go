package mgmt

import (
	"context"
	"net/http"
	"testing"
)

// TestResolveGroupIDsUsesExactName pins the name-to-id resolution the
// grant path relies on: a search hit whose name is not exactly the
// requested string must not be taken.
func TestResolveGroupIDsUsesExactName(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathGroups {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
			return
		}
		if got := r.URL.Query().Get("name"); got != "云平台测试" {
			t.Errorf("name filter = %q, want the exact name", got)
		}
		_, _ = w.Write([]byte(`{"count":2,"results":[
			{"id":"g1","name":"云平台测试","users_amount":1},
			{"id":"g2","name":"云平台测试-旧","users_amount":3}
		]}`))
	})

	ids, err := ResolveGroupIDs(context.Background(), client, []string{"云平台测试"})
	if err != nil {
		t.Fatalf("ResolveGroupIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "g1" {
		t.Errorf("ids = %v, want [g1]", ids)
	}
}

func TestResolveGroupIDsRefusesUnknownName(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
	})

	if _, err := ResolveGroupIDs(context.Background(), client, []string{"nope"}); err == nil {
		t.Fatal("an unknown group name resolved silently")
	}
}

func TestResolveGroupIDsRefusesAmbiguousName(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count":2,"results":[
			{"id":"g1","name":"ops"},
			{"id":"g2","name":"ops"}
		]}`))
	})

	if _, err := ResolveGroupIDs(context.Background(), client, []string{"ops"}); err == nil {
		t.Fatal("an ambiguous group name resolved to the first match")
	}
}

// TestResolveIDsReadsUUIDsByDetailRoute validates an id the caller names
// explicitly; the detail route is also the only lookup that can tell a
// real id from a UUID-shaped name.
func TestResolveIDsReadsUUIDsByDetailRoute(t *testing.T) {
	t.Parallel()

	const id = "11111111-2222-3333-4444-555555555555"
	gets := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathGroups+id+"/" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
			return
		}
		gets++
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"云平台运维"}`))
	})
	ids, err := ResolveGroupIDs(context.Background(), client, []string{id})
	if err != nil {
		t.Fatalf("ResolveGroupIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Errorf("ids = %v, want [%s]", ids, id)
	}
	if gets != 1 {
		t.Errorf("detail GETs = %d, want 1", gets)
	}
}

// TestResolveIDsFallsBackForUUIDShapedName covers a legal username or
// group name that happens to look like a UUID: the detail route 404s and
// the name filter still finds it.
func TestResolveIDsFallsBackForUUIDShapedName(t *testing.T) {
	t.Parallel()

	const ref = "11111111-2222-3333-4444-555555555555"
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathGroups+ref+"/" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
			return
		}
		if r.URL.Path != PathGroups {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("name"); got != ref {
			t.Errorf("name filter = %q", got)
		}
		_, _ = w.Write([]byte(`{"count":1,"results":[
			{"id":"g-real","name":"` + ref + `"}]}`))
	})

	ids, err := ResolveGroupIDs(context.Background(), client, []string{ref})
	if err != nil {
		t.Fatalf("ResolveGroupIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "g-real" {
		t.Errorf("ids = %v, want [g-real]", ids)
	}
}

func TestResolveUserIDsMatchesUsernameOnly(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathUsers {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
			return
		}
		if got := r.URL.Query().Get("username"); got != "mifazhan" {
			t.Errorf("username filter = %q", got)
		}
		_, _ = w.Write([]byte(`{"count":2,"results":[
			{"id":"u1","username":"mifazhan","name":"米法展"},
			{"id":"u2","username":"other","name":"mifazhan"}
		]}`))
	})

	ids, err := ResolveUserIDs(context.Background(), client, []string{"mifazhan"})
	if err != nil {
		t.Fatalf("ResolveUserIDs: %v", err)
	}
	// The second record merely carries the same display name; only the
	// username counts.
	if len(ids) != 1 || ids[0] != "u1" {
		t.Errorf("ids = %v, want [u1]", ids)
	}
}

// TestGetIdentityGroupsIncludesMembers covers the read gap behind
// "group rules are invisible": a name lookup goes through the list view,
// which drops users, so the detail has to be read afterwards.
func TestGetIdentityGroupsIncludesMembers(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathGroups:
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":"g1","name":"云平台测试","users_amount":1}
			]}`))
		case PathGroups + "g1/":
			_, _ = w.Write([]byte(`{"id":"g1","name":"云平台测试","users":[
				{"id":"u1","name":"米法展","is_service_account":false}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
		}
	})

	one, err := GetIdentity(context.Background(), client, IdentGroups, "云平台测试")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	group, ok := one.(Group)
	if !ok {
		t.Fatalf("resolved %T, want Group", one)
	}
	if len(group.Users) != 1 || group.Users[0].ID != "u1" || group.Users[0].Name != "米法展" {
		t.Errorf("users = %+v, want u1/米法展", group.Users)
	}
	// The detail serializer drops users_amount; the count from the list
	// record must survive the replacement.
	if group.UsersCount != 1 {
		t.Errorf("users_amount = %d, want the list count 1", group.UsersCount)
	}
}

// TestLooksLikeUUID pins the shape check the resolver uses to decide
// whether an id can be taken as-is.
func TestLooksLikeUUID(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"11111111-2222-3333-4444-555555555555": true,
		"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE": true,
		"11111111-2222-3333-4444-55555555555":  false,
		"云平台测试":                                false,
		"not-a-uuid":                           false,
	}
	for in, want := range cases {
		if got := looksLikeUUID(in); got != want {
			t.Errorf("looksLikeUUID(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestResolveGroupIDsKeepsFirstOccurrence documents the de-duplication
// that keeps a repeated flag from sending the same id twice; the second
// occurrence also skips its lookup.
func TestResolveGroupIDsKeepsFirstOccurrence(t *testing.T) {
	t.Parallel()
	const id = "11111111-2222-3333-4444-555555555555"
	gets := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gets++
		if r.URL.Path != PathGroups+id+"/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"ops"}`))
	})
	ids, err := ResolveGroupIDs(context.Background(), client, []string{id, id})
	if err != nil {
		t.Fatalf("ResolveGroupIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("ids = %v, want one entry", ids)
	}
	if gets != 1 {
		t.Errorf("GETs = %d, want 1 for a repeated id", gets)
	}
}

// TestGetIdentityGroupByIDDoesNotReread pins the other half of the group
// read: an id lookup already returns the detail (the same serializer
// that carries users), so it must not issue a second request. If a build
// ever dropped users from the detail, this test would fail loudly.
func TestGetIdentityGroupByIDDoesNotReread(t *testing.T) {
	t.Parallel()

	gets := 0
	const id = "11111111-2222-3333-4444-555555555555"
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathGroups+id+"/" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
			return
		}
		gets++
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"云平台测试","users":[
			{"id":"u1","name":"米法展","is_service_account":false}
		]}`))
	})

	one, err := GetIdentity(context.Background(), client, IdentGroups, id)
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	group, ok := one.(Group)
	if !ok {
		t.Fatalf("resolved %T, want Group", one)
	}
	if len(group.Users) != 1 || group.Users[0].ID != "u1" {
		t.Errorf("users = %+v, want u1", group.Users)
	}
	if gets != 1 {
		t.Errorf("detail GETs = %d, want 1", gets)
	}
}

// TestResolveRefusesBlank keeps an empty flag from being read as "no
// principal given", which would otherwise fall back to the caller.
func TestResolveRefusesBlank(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for a blank reference: %s", r.URL.Path)
	})
	if _, err := ResolveGroupIDs(context.Background(), client, []string{"  "}); err == nil {
		t.Fatal("a blank group reference resolved silently")
	}
	if _, err := ResolveUserIDs(context.Background(), client, []string{""}); err == nil {
		t.Fatal("a blank user reference resolved silently")
	}
}
