package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/config"
)

// seedLSFixture configures one server with a stored credential and points
// the fake session's API client at a local asset server.
func seedLSFixture(t *testing.T, env *cliEnv, internal, external string,
	items []map[string]any, record func(*http.Request)) {

	t.Helper()
	env.seedServer("bastion", internal, external, "testuser")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	server := newAssetServer(t, items, record)
	env.clientURL = server.URL
}

func TestLSPrintsAssetTableAndReportsEndpoint(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "",
		[]map[string]any{
			assetRow("id-1", "web-01", "10.0.0.1", "Linux"),
			assetRow("id-2", "db-01", "10.0.0.2", "MySQL"),
		}, nil)

	if code := env.run("ls"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	stdout := env.stdout()
	for _, want := range []string{"Name", "Address", "Platform", "Type", "web-01", "10.0.0.1", "Linux", "db-01", "Total: 2 asset(s)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(env.stderr(), "internal") {
		t.Errorf("stderr = %q, want the chosen endpoint reported", env.stderr())
	}
	if strings.Contains(stdout, "Using") {
		t.Errorf("stdout = %q, want the endpoint report on stderr so stdout stays parseable", stdout)
	}
}

func TestLSNoAssetsExitsZero(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "", []map[string]any{}, nil)

	if code := env.run("ls"); code != 0 {
		t.Fatalf("exit code = %d, want 0 for an empty listing (stderr: %s)", code, env.stderr())
	}
	if !strings.Contains(env.stdout(), "No assets found.") {
		t.Fatalf("stdout = %q, want the empty-listing message", env.stdout())
	}
}

func TestLSSearchUsesTheSearchQuery(t *testing.T) {
	var gotQuery string
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "",
		[]map[string]any{assetRow("id-1", "web-01", "10.0.0.1", "Linux")},
		func(r *http.Request) { gotQuery = r.URL.RawQuery })

	if code := env.run("ls", "-q", "web"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if !strings.Contains(gotQuery, "search=web") {
		t.Fatalf("query = %q, want it to carry search=web", gotQuery)
	}
	if !strings.Contains(env.stdout(), "web-01") {
		t.Fatalf("stdout = %q, want the search result", env.stdout())
	}
}

func TestLSLimitCapsTheResults(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "", assetRows(5), nil)

	if code := env.run("ls", "-n", "2"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if !strings.Contains(env.stdout(), "Total: 2 asset(s)") {
		t.Fatalf("stdout = %q, want the listing capped at 2", env.stdout())
	}
	if strings.Contains(env.stdout(), "asset-e") {
		t.Fatalf("stdout = %q, want the fifth asset excluded by -n", env.stdout())
	}
}

func TestLSNamedServerUsesThatServer(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("prod", "http://prod.example:2280/", "", "alice")
	env.seedServer("dev", "http://dev.example:2280/", "", "bob")
	env.setDefaultServer("prod")
	for _, alias := range []string{"prod", "dev"} {
		if err := env.creds.Set(alias, config.CredPassword, "stored"); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
	}
	env.clientURL = newAssetServer(t, []map[string]any{
		assetRow("id-1", "dev-only", "10.0.0.9", "Linux"),
	}, nil).URL

	if code := env.run("ls", "dev"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if len(env.loggedIn) != 1 || !strings.Contains(env.loggedIn[0], "dev.example") {
		t.Fatalf("logged in to %v, want the named server", env.loggedIn)
	}
}

func TestLSUnknownServerFails(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "", assetRows(1), nil)

	code := env.run("ls", "bogus")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unknown server")
	}
	if !strings.Contains(env.stderr(), "server not found") {
		t.Fatalf("stderr = %q, want a not-found message", env.stderr())
	}
}

func TestLSEndpointInternalForcesInternalWithoutProbingExternal(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "http://external.example:2280/",
		[]map[string]any{assetRow("id-1", "web-01", "10.0.0.1", "Linux")}, nil)

	if code := env.run("ls", "--endpoint", "internal"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if len(env.probed) != 1 || !strings.Contains(env.probed[0], "internal") {
		t.Fatalf("probed %v, want only the forced internal address", env.probed)
	}
	if len(env.loggedIn) != 1 || !strings.Contains(env.loggedIn[0], "internal") {
		t.Fatalf("logged in to %v, want the forced internal address", env.loggedIn)
	}
	if !strings.Contains(env.stderr(), "internal") {
		t.Errorf("stderr = %q, want the internal endpoint reported", env.stderr())
	}
}

func TestLSEndpointExternalForcesExternalWithoutProbingInternal(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "http://external.example:2280/",
		[]map[string]any{assetRow("id-1", "web-01", "10.0.0.1", "Linux")}, nil)

	if code := env.run("ls", "--endpoint", "external"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if len(env.probed) != 1 || !strings.Contains(env.probed[0], "external") {
		t.Fatalf("probed %v, want only the forced external address", env.probed)
	}
}

func TestLSEndpointInternalOnExternalOnlyServerFailsWithoutFallback(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "", "http://external.example:2280/",
		[]map[string]any{assetRow("id-1", "web-01", "10.0.0.1", "Linux")}, nil)

	code := env.run("ls", "--endpoint", "internal")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when the forced address is not configured")
	}
	if !strings.Contains(env.stderr(), "no internal address configured") {
		t.Fatalf("stderr = %q, want it to name the missing address", env.stderr())
	}
	if !strings.Contains(env.stderr(), "refusing to fall back") {
		t.Fatalf("stderr = %q, want an explicit no-fallback statement", env.stderr())
	}
	if len(env.probed) != 0 {
		t.Fatalf("probed %v, want no probe of the external address", env.probed)
	}
	if len(env.loggedIn) != 0 {
		t.Fatalf("logged in to %v, want no fallback login", env.loggedIn)
	}
	if strings.Contains(env.stdout(), "web-01") {
		t.Fatalf("stdout = %q, want no listing when the forced endpoint is missing", env.stdout())
	}
}

func TestLSEndpointBogusIsAUsageError(t *testing.T) {
	env := newCLIEnv(t)
	seedLSFixture(t, env, "http://internal.example:2280/", "", assetRows(1), nil)

	code := env.run("ls", "--endpoint", "bogus")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an invalid --endpoint value")
	}
	stderr := env.stderr()
	for _, want := range []string{"invalid --endpoint", "internal", "external"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
	if len(env.probed) != 0 {
		t.Fatalf("probed %v, want no probe for an invalid flag value", env.probed)
	}
}

func TestLSFailsWhenTheCredentialIsMissing(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")

	code := env.run("ls")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when no password is stored")
	}
	if !strings.Contains(env.stderr(), "no stored password") {
		t.Fatalf("stderr = %q, want an actionable missing-credential message", env.stderr())
	}
	envName, envErr := config.EnvVarName("bastion", config.CredPassword)
	if envErr != nil {
		t.Fatalf("EnvVarName = %v", envErr)
	}
	if !strings.Contains(env.stderr(), envName) {
		t.Fatalf("stderr = %q, want it to name the environment fallback variable", env.stderr())
	}
}
