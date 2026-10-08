package cli

import (
	"strings"
	"testing"
)

// TestEffectiveVersionPrefersInjected asserts a release build wins: when
// ldflags injected a version, that is what the user sees.
func TestEffectiveVersionPrefersInjected(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })

	Version = "v1.2.3"
	if got := effectiveVersion(); got != "v1.2.3" {
		t.Fatalf("effectiveVersion() = %q, want the injected version", got)
	}
}

// TestEffectiveVersionFallsBackToBuildInfo pins the reason this fallback
// exists: `go install ...@latest` cannot pass ldflags, so without reading
// the module version every such user would be told they run "0.1.0-dev".
func TestEffectiveVersionFallsBackToBuildInfo(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })

	// The test binary is built by `go test`, which does not stamp a module
	// version, so moduleVersion is expected to be empty here. The assertion
	// is that the fallback chain still terminates at a usable string rather
	// than an empty one.
	Version = devVersion
	got := effectiveVersion()
	if got == "" {
		t.Fatal("effectiveVersion() returned an empty string")
	}
	if got != devVersion && got != moduleVersion() {
		t.Fatalf("effectiveVersion() = %q, want either the module version or the default", got)
	}
}

// TestVersionCommandReportsSomething asserts `jms version` never prints a
// blank version, whichever build produced the binary.
func TestVersionCommandReportsSomething(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })
	Version = "v9.9.9"

	env := newCLIEnv(t)
	if code := env.run("version"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	out := env.stdout()
	if !strings.Contains(out, "v9.9.9") {
		t.Fatalf("stdout = %q, want it to contain the version", out)
	}
	if !strings.Contains(out, "windows") && !strings.Contains(out, "linux") &&
		!strings.Contains(out, "darwin") {
		t.Fatalf("stdout = %q, want it to contain the platform", out)
	}
}
