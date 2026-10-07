package cli

import (
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/xfer"
)

func TestSftpParsesBothSidesAndInfersDirection(t *testing.T) {
	t.Run("upload local to remote", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)
		fakes.transferRes = xfer.Result{Bytes: 12}

		code := fakes.runWithRuntime(env, "sftp", "./report.pdf", "web1@bastion:/srv/report.pdf")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
		}
		req := fakes.transferCalls[0]
		if req.Task.Direction != xfer.Upload {
			t.Errorf("Direction = %q, want upload", req.Task.Direction)
		}
		if req.Task.Local != "./report.pdf" || req.Task.Remote != "/srv/report.pdf" {
			t.Errorf("paths = local %q remote %q, want ./report.pdf and /srv/report.pdf",
				req.Task.Local, req.Task.Remote)
		}
		if req.Asset != "web1" {
			t.Errorf("Asset = %q, want web1", req.Asset)
		}
	})

	t.Run("download remote to local", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)

		code := fakes.runWithRuntime(env, "sftp", "web1:/var/log/app.log", "./app.log")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
		}
		req := fakes.transferCalls[0]
		if req.Task.Direction != xfer.Download {
			t.Errorf("Direction = %q, want download", req.Task.Direction)
		}
		if req.Task.Local != "./app.log" || req.Task.Remote != "/var/log/app.log" {
			t.Errorf("paths = local %q remote %q", req.Task.Local, req.Task.Remote)
		}
	})

	t.Run("both sides local is an error", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)

		code := fakes.runWithRuntime(env, "sftp", "a.txt", "b.txt")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(env.stderr(), "remote path") {
			t.Fatalf("stderr = %q, want guidance toward <asset>:<path>", env.stderr())
		}
		if len(fakes.transferCalls) != 0 {
			t.Fatalf("Transfer reached with unparseable arguments")
		}
	})
}

func TestSftpVerifyDefaultsTrueAndNoVerifyTurnsItOff(t *testing.T) {
	t.Run("default verifies", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)

		if code := fakes.runWithRuntime(env, "sftp", "web1:/srv/f.bin", "./f.bin"); code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
		}
		if !fakes.transferCalls[0].Task.Verify {
			t.Errorf("Verify = false, want the default true")
		}
	})

	t.Run("--no-verify skips it", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)

		if code := fakes.runWithRuntime(env, "sftp", "--no-verify", "web1:/srv/f.bin", "./f.bin"); code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
		}
		if fakes.transferCalls[0].Task.Verify {
			t.Errorf("Verify = true, want false after --no-verify")
		}
	})
}

func TestSftpParallelAndChunkReachTask(t *testing.T) {
	env, fakes := newRuntimeEnv(t)

	code := fakes.runWithRuntime(env, "sftp", "--parallel", "8", "--chunk", "262144",
		"web1:/srv/f.bin", "./f.bin")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	task := fakes.transferCalls[0].Task
	if task.Parallel != 8 {
		t.Errorf("Parallel = %d, want 8", task.Parallel)
	}
	if task.Chunk != 262144 {
		t.Errorf("Chunk = %d, want 262144", task.Chunk)
	}
}

func TestSftpMissingCredentialFailsBeforeTransfer(t *testing.T) {
	env, fakes := newRuntimeEnv(t)
	if err := env.creds.Delete("bastion"); err != nil {
		t.Fatalf("delete credential: %v", err)
	}

	code := fakes.runWithRuntime(env, "sftp", "web1:/srv/f.bin", "./f.bin")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "no stored password") {
		t.Fatalf("stderr = %q, want credential guidance", env.stderr())
	}
	if len(fakes.transferCalls) != 0 {
		t.Fatalf("Transfer reached without credentials")
	}
}

func TestSftpUnsupportedShapesFailWithSeamLimit(t *testing.T) {
	t.Run("remote to remote", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)

		code := fakes.runWithRuntime(env, "sftp", "src:/a", "dst:/b")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(env.stderr(), "requires a transfer seam that carries two resolved sessions") {
			t.Fatalf("stderr = %q, want the ErrTransferSeamLimit guidance", env.stderr())
		}
		if len(fakes.transferCalls) != 0 {
			t.Fatalf("Transfer reached for a relay shape")
		}
	})

	t.Run("recursive", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)

		code := fakes.runWithRuntime(env, "sftp", "-R", "./dir", "dst-host:/dir")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero")
		}
		if !strings.Contains(env.stderr(), "requires a transfer seam") {
			t.Fatalf("stderr = %q, want the ErrTransferSeamLimit guidance", env.stderr())
		}
		if len(fakes.transferCalls) != 0 {
			t.Fatalf("Transfer reached for a recursive request")
		}
	})
}

func TestSftpVerifyAndNoVerifyAreMutuallyExclusive(t *testing.T) {
	env, fakes := newRuntimeEnv(t)

	code := fakes.runWithRuntime(env, "sftp", "--verify", "--no-verify", "web1:/a", "./a")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "mutually exclusive") {
		t.Fatalf("stderr = %q, want the exclusion message", env.stderr())
	}
}

func TestParseRemoteSpecShapes(t *testing.T) {
	cases := []struct {
		spec          string
		asset, server string
		path          string
		wantErr       bool
	}{
		{spec: "web1:/srv/x", asset: "web1", path: "/srv/x"},
		{spec: "web1@prod:/srv/x", asset: "web1", server: "prod", path: "/srv/x"},
		{spec: "10.0.0.1:~/x:y", asset: "10.0.0.1", path: "~/x:y"}, // first colon splits
		{spec: "web1@pro:d/srv", asset: "web1", server: "pro", path: "d/srv"},
		{spec: "web1:", wantErr: true},
		{spec: ":/srv/x", wantErr: true},
		{spec: "nocolon", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseRemoteSpec(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRemoteSpec(%q) = %+v, want error", tc.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRemoteSpec(%q) failed: %v", tc.spec, err)
			continue
		}
		if got.target.Asset != tc.asset || got.target.Server != tc.server || got.Path != tc.path {
			t.Errorf("parseRemoteSpec(%q) = %+v, want asset %q server %q path %q",
				tc.spec, got, tc.asset, tc.server, tc.path)
		}
	}
}

func TestIsRemoteSideWindowsDriveLetter(t *testing.T) {
	if isRemoteSide(`C:\Users\ops\data.csv`) {
		t.Errorf("a Windows drive path must stay local")
	}
	if !isRemoteSide("web1:/srv/data.csv") {
		t.Errorf("a remote spec must stay remote")
	}
}
