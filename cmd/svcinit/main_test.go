package main_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	svcinit "rules_itest/cmd/svcinit"
)

const child = "//:child"

// Bound a missing report or a supervisor that fails to exit after a read error.
const supervisorTimeout = 30 * time.Second

func TestTruncatedIBazelNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), supervisorTimeout)
	defer cancel()
	cmd := ibazelCommand(t, ctx)
	// EOF in the middle of a success message is not a successful build.
	cmd.Stdin = strings.NewReader(strings.TrimSuffix(svcinit.IBazelBuildSucceeded, "\n"))
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("svcinit did not exit on the read error: %v\n%s", ctx.Err(), output)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) ||
		!strings.Contains(string(output), "reading iBazel notifications: unexpected EOF") {
		t.Fatalf("want svcinit to reject a truncated notification; got %v:\n%s", err, output)
	}
}

func ibazelCommand(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()
	binary, err := runfiles.Rlocation(os.Getenv("SVCINIT_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, binary)
	if runtime.GOOS != "windows" {
		// Ctrl-C lets svcinit clean up its socket directory. Windows cannot
		// send os.Interrupt, so leave CommandContext's default kill in place.
		cmd.Cancel = func() error {
			return cmd.Process.Signal(os.Interrupt)
		}
	}
	// Allow graceful shutdown before os/exec falls back to killing svcinit.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(
		os.Environ(),
		"IBAZEL_NOTIFY_CHANGES=y",
		"SVCINIT_TERSE_OUTPUT=False",
		"TEST_TARGET="+child,
		"SVCINIT_SERVICE_SPECS_RLOCATION_PATH="+empty,
		"SVCINIT_TEST_ENV_RLOCATION_PATH="+empty,
	)
	return cmd
}
