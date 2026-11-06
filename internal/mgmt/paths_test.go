package mgmt

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestValidateReadPath(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"/api/v1/audits/login-logs/",
		"/api/v1/audits/operate-logs/",
		"/api/v1/terminal/sessions/",
		"/api/v1/terminal/sessions/e710459d-af1b-4e21-a292-da57250d5a38/",
		"/api/v1/assets/nodes/",
		"/api/v1/accounts/accounts/",
		"/api/v1/users/profile/",
		"/api/v1/rbac/roles/",
		"/api/v1/orgs/orgs/",
		"/api/v1/ops/jobs/",
		"/api/v1/tickets/tickets/",
		"/api/v1/settings/",
	}
	for _, path := range allowed {
		if _, err := ValidateReadPath(path); err != nil {
			t.Errorf("ValidateReadPath(%q) = %v, want nil", path, err)
		}
	}

	refused := []string{
		"",
		"api/v1/assets/nodes/", // no leading slash
		"https://bastion.example.com/api/v1/assets/", // absolute URL
		"//bastion.example.com/api/v1/assets/",       // protocol-relative
		"/api/v1/authentication/auth/",               // credential segment
		"/api/v1/users/1/secrets/",                   // credential segment
		"/api/v1/accounts/accounts/?limit=1",         // query belongs in -q
		"/api/v1/assets/../../etc/passwd",            // traversal
		"/api/v1/assets//nodes/",                     // empty segment
		"/api/v2/assets/nodes/",                      // wrong API version
		"/api/v1/",                                   // the root itself
		"/api/v1/assets",                             // not a collection path
		"/api/v1/nothing-here/",                      // outside the whitelist
		strings.Repeat("/api/v1/assets/", 100),       // longer than the bound
		"/api/v1/terminal/sessions/\x00/",            // control byte
	}
	for _, path := range refused {
		if _, err := ValidateReadPath(path); err == nil {
			t.Errorf("ValidateReadPath(%q) = nil, want a refusal", path)
		} else if !errors.Is(err, ErrPathNotAllowed) {
			t.Errorf("ValidateReadPath(%q) error = %v, want ErrPathNotAllowed", path, err)
		}
	}
}

func TestReadablePrefixesAreSortedAndDistinct(t *testing.T) {
	t.Parallel()

	prefixes := ReadablePrefixes()
	if len(prefixes) == 0 {
		t.Fatal("no readable prefixes")
	}
	seen := map[string]bool{}
	for i, p := range prefixes {
		if !strings.HasPrefix(p, "/api/") || !strings.HasSuffix(p, "/") {
			t.Errorf("prefix %q is not a /api/.../ shape", p)
		}
		if seen[p] {
			t.Errorf("prefix %q appears twice", p)
		}
		seen[p] = true
		if i > 0 && prefixes[i-1] > p {
			t.Errorf("prefixes are not sorted: %q before %q", prefixes[i-1], p)
		}
	}
}

func TestMaskQuery(t *testing.T) {
	t.Parallel()

	values := url.Values{
		"limit":    {"20"},
		"password": {"hunter2"},
		"q":        {strings.Repeat("x", 500)},
	}
	got := MaskQuery(values)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("MaskQuery leaked a password: %q", got)
	}
	if !strings.Contains(got, "password=***") {
		t.Errorf("MaskQuery did not mark the password: %q", got)
	}
	if !strings.Contains(got, "limit=20") {
		t.Errorf("MaskQuery dropped an ordinary parameter: %q", got)
	}
	if len(got) > 400 {
		t.Errorf("MaskQuery did not bound the value length: %d bytes", len(got))
	}
	if MaskQuery(nil) != "" {
		t.Errorf("MaskQuery(nil) should be empty")
	}
}

func TestClampLimit(t *testing.T) {
	t.Parallel()

	cases := map[int]int{-5: DefaultLimit, 0: DefaultLimit, 1: 1, 20: 20, MaxLimit: MaxLimit, MaxLimit + 1: MaxLimit}
	for in, want := range cases {
		if got := ClampLimit(in); got != want {
			t.Errorf("ClampLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildTreeGroupsByPath(t *testing.T) {
	t.Parallel()

	flat := []Node{
		{ID: "1", Value: "Default", FullVal: "Default"},
		{ID: "2", Value: "生产", FullVal: "Default/生产"},
		{ID: "3", Value: "预演", FullVal: "Default/生产/预演"},
		{ID: "4", Value: "Orphan", FullVal: "Missing/Orphan"},
	}
	tree := BuildTree(flat)
	if len(tree) != 2 {
		t.Fatalf("roots = %d, want 2 (Default and the orphan): %+v", len(tree), tree)
	}
	root := tree[0]
	if root.ID != "1" || len(root.Children) != 1 {
		t.Fatalf("root = %+v, want Default with one child", root)
	}
	if got := root.Children[0]; got.ID != "2" || len(got.Children) != 1 || got.Children[0].ID != "3" {
		t.Fatalf("subtree = %+v, want 2 -> 3", got)
	}
}
