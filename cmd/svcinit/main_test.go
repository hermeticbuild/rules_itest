package main_test

import (
	"bufio"
	"context"
	"errors"
	"io"
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

func TestRerunAfterSuccessfulBuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), supervisorTimeout)
	cmd := ibazelCommand(t, ctx)
	// svcinit stays up after the last report. Cancel it before waiting for exit.
	defer cmd.Wait()
	defer cancel()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Launch logs go to stderr, timing reports to stdout. Their order matters.
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The child (exit0) finishes on its own. Send no builds until svcinit reports it.
	output := bufio.NewReader(stdout)
	readThroughReport(t, output)

	const builds = svcinit.IBazelBuildStarted + svcinit.IBazelBuildFailed +
		svcinit.IBazelBuildStarted + svcinit.IBazelBuildSucceeded
	if _, err := io.WriteString(stdin, builds); err != nil {
		t.Fatal(err)
	}

	// Between the two reports there must be just one launch, after the success
	// notification. Any earlier launch means svcinit reran without a good build.
	logs := readThroughReport(t, output)
	before, after, found := strings.Cut(logs, "\n"+svcinit.IBazelBuildSucceeded)
	const launch = "Executing test:"
	if !found || strings.Contains(before, launch) || strings.Count(after, launch) != 1 {
		t.Fatalf("want one test launch after the successful build; got:\n%s", logs)
	}
}

// Return everything svcinit writes through the child's row in the timing report.
func readThroughReport(t *testing.T, output *bufio.Reader) string {
	t.Helper()
	var logs strings.Builder
	for {
		line, err := output.ReadString('\n')
		logs.WriteString(line)
		if err != nil {
			t.Fatalf("reading %s report: %v\n%s", child, err, logs.String())
		}
		if strings.HasPrefix(line, child+" ") {
			return logs.String()
		}
	}
}

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
