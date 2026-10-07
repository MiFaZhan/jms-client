package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// ErrTransferSeamLimit reports a transfer shape the frozen CLI transfer
// seam cannot carry yet (asset-to-asset relay, recursive copy). The seam
// is TransferRequest: one resolved session plus one xfer.Task; a transfer
// needing two sessions or a directory walk needs a wider seam first, so
// callers get the honest boundary instead of a silent workaround.
var ErrTransferSeamLimit = errors.New(
	"requires a transfer seam that carries two resolved sessions (relay) or a directory walk (-R)")

// sftpOptions carries the parsed `jms sftp` flags.
type sftpOptions struct {
	recursive bool
	verify    bool
	noVerify  bool
	parallel  int
	chunk     int
	endpoint  string
	account   string
}

// newSFTPCommand builds `jms sftp <src> <dst>`.
//
// One or both sides may be <asset>[@<server>]:<path>; the direction is
// inferred from which sides name a remote host.
func newSFTPCommand(deps *Deps) *cobra.Command {
	var opts sftpOptions
	cmd := &cobra.Command{
		Use:   "sftp <src> <dst>",
		Short: "Copy a file to or from an asset",
		Long: `Copy a file between the local machine and an asset, or between two
assets (streaming relay).

Exactly one or both sides must be a remote spec <asset>[@<server>]:<path>;
the direction is inferred from which sides are remote. Two local paths are
an error: use cp.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSFTP(cmd, *deps, args[0], args[1], opts)
		},
	}
	cmd.Flags().BoolVarP(&opts.recursive, "recursive", "R", false, "Recurse into directories")
	cmd.Flags().BoolVar(&opts.verify, "verify", false, "Verify with md5 (default)")
	cmd.Flags().BoolVar(&opts.noVerify, "no-verify", false, "Skip md5 verification")
	cmd.Flags().IntVar(&opts.parallel, "parallel", 0, "Concurrent chunk streams")
	cmd.Flags().IntVar(&opts.chunk, "chunk", 0, "Chunk size in bytes")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().StringVar(&opts.account, "account", "", "Account override")
	return cmd
}

// remoteSpec is a parsed <asset>[@<server>]:<path> sftp argument.
type remoteSpec struct {
	target commandTarget
	Path   string
}

// parseRemoteSpec splits <asset>[@<server>]:<path>.
//
// The FIRST colon splits the host part from the path, so the path itself
// may contain further colons (scp semantics). Windows drive letters are
// not a concern on the remote side: remote paths are POSIX.
func parseRemoteSpec(spec string) (remoteSpec, error) {
	colon := strings.Index(spec, ":")
	if colon <= 0 {
		return remoteSpec{}, fmt.Errorf(
			"invalid remote spec %q: expected <asset>[@<server>]:<path>", spec)
	}
	host, path := spec[:colon], spec[colon+1:]
	t, err := parseCommandTarget(host)
	if err != nil {
		return remoteSpec{}, fmt.Errorf("invalid remote spec %q: %w", spec, err)
	}
	if strings.TrimSpace(path) == "" {
		return remoteSpec{}, fmt.Errorf(
			"invalid remote spec %q: remote path is empty", spec)
	}
	return remoteSpec{target: t, Path: path}, nil
}

// isRemoteSide reports whether an sftp argument names a remote host.
//
// The presence of a colon is the discriminator, matching the reference
// implementation — except for a Windows drive letter (C:\...), which is
// always local on this codebase's first-class platform.
func isRemoteSide(arg string) bool {
	if isWindowsDrivePath(arg) {
		return false
	}
	return strings.Contains(arg, ":")
}

// isWindowsDrivePath reports whether arg starts with a drive letter
// followed by a colon (C:\ or C:/).
func isWindowsDrivePath(arg string) bool {
	if len(arg) < 2 || arg[1] != ':' {
		return false
	}
	c := arg[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// parsedTransfer is one xfer.Task plus the remote side it was parsed
// from, so the caller does not re-parse the argument.
type parsedTransfer struct {
	task  xfer.Task
	asset commandTarget
}

// parseTransferTask decides the direction and builds the xfer.Task.
//
// Both sides remote means a streaming relay between two assets. The frozen
// CLI transfer seam carries one session per call (TransferRequest), so the
// relay shape is not expressible through it yet; the error says so and
// names what WOULD express it rather than silently copying through a
// local temp file.
func parseTransferTask(src, dst string) (parsedTransfer, error) {
	srcRemote, dstRemote := isRemoteSide(src), isRemoteSide(dst)
	switch {
	case !srcRemote && !dstRemote:
		return parsedTransfer{}, fmt.Errorf(
			"neither argument is a remote path: use <asset>[@<server>]:<path> for the remote side")
	case srcRemote && dstRemote:
		return parsedTransfer{}, fmt.Errorf("asset-to-asset relay: %w", ErrTransferSeamLimit)
	case dstRemote:
		spec, err := parseRemoteSpec(dst)
		if err != nil {
			return parsedTransfer{}, err
		}
		return parsedTransfer{
			task:  xfer.Task{Direction: xfer.Upload, Local: src, Remote: spec.Path},
			asset: spec.target,
		}, nil
	default:
		spec, err := parseRemoteSpec(src)
		if err != nil {
			return parsedTransfer{}, err
		}
		return parsedTransfer{
			task:  xfer.Task{Direction: xfer.Download, Local: dst, Remote: spec.Path},
			asset: spec.target,
		}, nil
	}
}

// runSFTP parses both sides, resolves the remote one and hands the task to
// the Runtime.Transfer seam, which owns the SFTP engine.
//
// Verification is on by default (DESIGN.md §7.2 of the reference CLI):
// --no-verify turns it off; the redundant --verify flag is accepted for
// symmetry with --no-verify and changes nothing.
func runSFTP(cmd *cobra.Command, deps Deps, src, dst string, opts sftpOptions) error {
	if opts.recursive {
		return fmt.Errorf("recursive directory transfer (-R): %w", ErrTransferSeamLimit)
	}
	if opts.verify && opts.noVerify {
		return fmt.Errorf("--verify and --no-verify are mutually exclusive")
	}

	parsed, err := parseTransferTask(src, dst)
	if err != nil {
		return err
	}
	task := parsed.task
	task.Verify = !opts.noVerify
	task.Chunk = opts.chunk
	task.Parallel = opts.parallel

	force, err := parseEndpointKind(opts.endpoint)
	if err != nil {
		return err
	}

	conn, err := resolveCommandTarget(cmd, deps, parsed.asset)
	if err != nil {
		return err
	}

	ctx := commandContext(cmd)
	session, kind, err := conn.connect(ctx, force)
	if err != nil {
		return err
	}

	_ = kind
	res, err := deps.Runtime.Transfer(ctx, TransferRequest{
		Session:  session,
		Asset:    parsed.asset.Asset,
		Account:  opts.account,
		Task:     task,
		Endpoint: string(force),
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(deps.Out, "Transferred %d bytes in %s\n", res.Bytes, res.Duration)
	if res.Verified {
		fmt.Fprintf(deps.Out, "MD5 verified: %s\n", res.MD5Remote)
	}
	return nil
}
