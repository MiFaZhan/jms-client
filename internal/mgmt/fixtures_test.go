package mgmt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures under testdata are the real record shapes returned by the
// reference JumpServer v4, with every value replaced by "x" and the page cut
// to one row.
//
// They exist because three separate字段 shapes failed in a row during the
// first end-to-end run: `status`/`type`/`action` arrive as {"value","label"}
// objects, `phone` as {"code","phone"}, `source` as an object, `error_reason`
// as an object. Every one of those was a decode error that made a working
// endpoint look broken. Values are irrelevant to that bug class; the shape is
// the whole test, so scrubbing the values loses nothing and keeps personal
// data out of the repository.
func TestFixtureShapesDecode(t *testing.T) {
	t.Parallel()

	auditFixtures := []struct {
		file string
		kind string
	}{
		{"login.json", AuditLogin},
		{"operate.json", AuditOperate},
		{"password.json", AuditPassword},
		{"ftp.json", AuditFTP},
		{"job.json", AuditJob},
	}
	for _, f := range auditFixtures {
		body := readFixture(t, f.file)
		var page struct {
			Results []rawAudit `json:"results"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Errorf("%s (%s): %v", f.file, f.kind, err)
			continue
		}
		for _, row := range page.Results {
			entry := normalizeAudit(f.kind, row)
			if entry.Kind != f.kind {
				t.Errorf("%s: kind = %q", f.file, entry.Kind)
			}
		}
	}

	cases := []struct {
		file string
		into any
	}{
		{"sessions.json", &struct {
			Results []Session `json:"results"`
		}{}},
		{"users.json", &struct {
			Results []User `json:"results"`
		}{}},
		{"accounts.json", &struct {
			Results []Account `json:"results"`
		}{}},
		{"nodes.json", &struct {
			Results []Node `json:"results"`
		}{}},
		{"roles.json", &struct {
			Results []Role `json:"results"`
		}{}},
		{"groups.json", &struct {
			Results []Group `json:"results"`
		}{}},
		{"orgs.json", &struct {
			Results []Org `json:"results"`
		}{}},
		{"platforms.json", &struct {
			Results []Platform `json:"results"`
		}{}},
		{"tickets.json", &struct {
			Results []Ticket `json:"results"`
		}{}},
		{"commands.json", &struct {
			Results []Command `json:"results"`
		}{}},
	}
	for _, tc := range cases {
		body := readFixture(t, tc.file)
		if err := json.Unmarshal(body, tc.into); err != nil {
			t.Errorf("%s: %v", tc.file, err)
		}
	}
}

// TestAssignedShapesDecode covers the shapes only the write paths touch.
//
// There is no fixture for these: nothing was created on the reference
// instance, so these bodies are the ones the code sends, decoded back into
// the structs it reads — a round-trip check that the request and the
// response models agree.
func TestAssignedShapesDecode(t *testing.T) {
	t.Parallel()

	var node Node
	if err := json.Unmarshal([]byte(`{"id":"n1","value":"Default","full_value":"Default","children":[]}`), &node); err != nil {
		t.Errorf("node: %v", err)
	}
	var account Account
	if err := json.Unmarshal([]byte(`{"id":"a1","asset":{"id":"x","name":"x"},"secret_type":{"value":"password"}}`), &account); err != nil {
		t.Errorf("account: %v", err)
	}
}

// readFixture loads one testdata file.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}
