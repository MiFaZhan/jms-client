package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// Local audit retention.
//
// The audit files are per-pid JSONL (audit-YYYYMMDD-<pid>.jsonl) and nothing
// used to remove them, so they accumulated one per process start. The window
// is a property of this machine rather than of a server, which is why it is a
// top-level configuration key instead of a [servers.*] field.
//
// It is enforced here, inside the tool, rather than by an external timer: a
// cron entry only covers the machine someone remembered to configure, and it
// drifts from the tool's own defaults.

// resolveAuditKeepDays resolves the retention window in days; 0 means "do not
// prune".
//
// Precedence: JMS_AUDIT_KEEP_DAYS, then audit_keep_days from the
// configuration file. An unreadable configuration is not an error — most
// commands never need it, and a missing window must not stop a command from
// running. An unparsable value is reported and treated as "off": a typo in
// "90" must not turn into "delete everything", and it must not silently
// become some other window either.
func resolveAuditKeepDays(cmd *cobra.Command, deps Deps) int {
	if raw := strings.TrimSpace(os.Getenv(obs.EnvAuditKeepDays)); raw != "" {
		days, err := obs.ParseKeepDays(raw)
		if err != nil {
			fmt.Fprintf(deps.Err, "jms: %s=%q ignored: %v\n", obs.EnvAuditKeepDays, raw, err)
			return 0
		}
		return days
	}
	// configPathFor (args.go) already resolves Deps.ConfigPath, --config,
	// JMS_CONFIG and the platform default, in that order.
	path, err := configPathFor(cmd, deps)
	if err != nil {
		return 0
	}
	cfg, err := config.Load(path)
	if err != nil || cfg == nil {
		return 0
	}
	return cfg.AuditKeepDays
}

// pruneAuditOnStartup applies the retention window at most once per day.
//
// It runs from the root command's PersistentPreRunE, so every command — a
// one-shot `jms exec` and a long-lived `jms mcp` host alike — enforces the
// same window. The 24-hour throttle lives in obs.PruneIfDue, so a burst of
// commands does not rescan the directory each time.
//
// Failures are reported on stderr and never fail the command: housekeeping is
// not worth breaking a command over.
func pruneAuditOnStartup(cmd *cobra.Command, deps Deps) {
	keep := resolveAuditKeepDays(cmd, deps)
	if keep <= 0 {
		return
	}
	dir := deps.Runtime.AuditDir
	if strings.TrimSpace(dir) == "" {
		return
	}
	result, ran, err := obs.PruneIfDue(dir, keep, deps.Now(), false)
	if err != nil {
		fmt.Fprintf(deps.Err, "jms: audit prune skipped: %v\n", err)
		return
	}
	if ran && result.Deleted > 0 {
		fmt.Fprintf(deps.Err, "jms: audit prune: %s\n", result)
	}
}
