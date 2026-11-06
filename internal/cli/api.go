package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/mgmt"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// newAPICommand builds `jms api`, the manual, read-only door onto the
// JumpServer management API.
//
// It exists for two jobs that would otherwise have no tool at all, and it
// is deliberately NOT mirrored into the MCP surface (MIFA-73, 方案 A 单轨):
//
//  1. Verifying that a management path is what this JumpServer actually
//     serves. Guesswork about REST paths is the main risk in adding管理面
//     tools, and there is no way to check a path without a GET.
//  2. Debugging one endpoint from a terminal on a day the MCP tool that
//     wraps it is the thing that is broken.
//
// The asymmetry with MCP is intentional: the caller here is a person at a
// keyboard who can read the refusal, whereas an MCP caller is a model that
// can be steered by a prompt. A `path` parameter on the model's side would
// make "read anything" one sentence away.
func newAPICommand(deps *Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "api",
		Short: "Read the management API by path (read-only, manual use)",
		Long: `Read one JumpServer management API path.

This is a read-only, allowlisted escape hatch for a human at a terminal: it
is how a management path gets verified against the real server before it is
wrapped in a tool, and how one endpoint is inspected when that tool is the
thing that is broken.

Only GET is possible; the path must fall under one of these prefixes:

  ` + strings.Join(mgmt.ReadablePrefixes(), "\n  ") + `

Credential-carrying segments (authentication, secret, token, key, password)
are refused even under an allowed prefix, and every call is recorded in the
audit stream as an api.read event.`,
	}
	group.AddCommand(newAPIGetCommand(deps))
	return group
}

// apiGetOptions carries the parsed `jms api get` flags.
type apiGetOptions struct {
	server   string
	endpoint string
	query    []string
	raw      bool
	maxBytes int
}

// newAPIGetCommand builds `jms api get`.
func newAPIGetCommand(deps *Deps) *cobra.Command {
	opts := apiGetOptions{maxBytes: 16384}
	cmd := &cobra.Command{
		Use:   "get <path> [server]",
		Short: "GET one management path and print the JSON",
		Example: `  jms api get /api/v1/audits/login-logs/ -q limit=5
  jms api get /api/v1/terminal/sessions/ -q limit=3 --raw`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				opts.server = args[1]
			}
			return runAPIGet(cmd, *deps, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().StringArrayVarP(&opts.query, "query", "q", nil, "Query parameter key=value (repeatable)")
	cmd.Flags().BoolVar(&opts.raw, "raw", false, "Print the response without indenting")
	cmd.Flags().IntVar(&opts.maxBytes, "max-bytes", 16384, "Truncate the printed body to this many bytes (0 = no limit)")
	return cmd
}

// runAPIGet validates the path, performs the GET and records the call.
func runAPIGet(cmd *cobra.Command, deps Deps, rawPath string, opts apiGetOptions) error {
	path, err := mgmt.ValidateReadPath(rawPath)
	if err != nil {
		// A refusal is a policy decision, not a server answer: record it so
		// an attempt to read outside the surface is visible, then fail.
		deps.publishAudit(obs.Event{
			Kind:   obs.KindAPIDenied,
			Path:   strings.TrimSpace(rawPath),
			Reason: err.Error(),
		})
		return err
	}

	params, err := parseQuery(opts.query)
	if err != nil {
		return err
	}

	client, err := assetSession(cmd, deps, opts.server, opts.endpoint)
	if err != nil {
		return err
	}

	started := deps.Now()
	var body json.RawMessage
	getErr := client.Get(commandContext(cmd), path, params, &body)

	event := obs.Event{
		Kind:       obs.KindAPIRead,
		Path:       path,
		Params:     mgmt.MaskQuery(params),
		DurationMS: deps.Now().Sub(started).Milliseconds(),
	}
	if getErr != nil {
		event.Error = getErr.Error()
		event.Status = statusOf(getErr)
		if event.Status == 0 {
			event.Kind = obs.KindError
		}
		deps.publishAudit(event)
		return getErr
	}
	event.Status = 200
	event.OutputBytes = len(body)
	deps.publishAudit(event)

	if len(body) == 0 {
		fmt.Fprintln(deps.Out, "No content.")
		return nil
	}
	printed := body
	if opts.raw {
		printed = compactJSON(body)
	} else {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err == nil {
			printed = buf.Bytes()
		}
	}
	if opts.maxBytes > 0 && len(printed) > opts.maxBytes {
		fmt.Fprintf(deps.Out, "%s\n… truncated at %d of %d bytes (use --max-bytes 0 to print all)\n",
			printed[:opts.maxBytes], opts.maxBytes, len(printed))
		return nil
	}
	fmt.Fprintf(deps.Out, "%s\n", printed)
	return nil
}

// parseQuery turns repeated key=value flags into url.Values.
func parseQuery(pairs []string) (url.Values, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	values := url.Values{}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("-q %q is not key=value", pair)
		}
		values.Add(strings.TrimSpace(key), value)
	}
	return values, nil
}

// statusOf extracts the HTTP status from an API error, or 0 when the
// request never produced a response.
func statusOf(err error) int {
	if e, ok := api.AsAPIError(err); ok {
		return e.StatusCode
	}
	if e, ok := api.AsAuthError(err); ok {
		return e.StatusCode
	}
	return 0
}

// compactJSON strips insignificant whitespace from a JSON body.
func compactJSON(body []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return body
	}
	return buf.Bytes()
}

// publishAudit records one event through the runtime bus.
//
// It is a no-op when auditing is off, which is the same behaviour the exec
// path has: `JMS_AUDIT=off` must not turn a command into a failure.
func (d Deps) publishAudit(e obs.Event) {
	if d.Runtime == nil || d.Runtime.Bus == nil {
		return
	}
	if e.TS.IsZero() {
		e.TS = d.Now()
	}
	if e.PID == 0 {
		e.PID = os.Getpid()
	}
	if e.Actor == "" {
		e.Actor = d.Runtime.Actor
	}
	d.Runtime.Bus.Publish(e)
}
