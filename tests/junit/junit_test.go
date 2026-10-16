package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

var svcinitRunfile = flag.String("svcinit", "", "Service runner runfile")

func markReady(path string) {
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		panic(err)
	}
}

// Reuse this test binary for child tests, probes, and services. Fixtures need
// no shell or host runtime. All intentionally surviving descendants are terminated.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		mode := os.Args[1]
		if path := os.Getenv("JUNIT_SOCKET_DIR_RECORD"); path != "" {
			if err := os.WriteFile(path, []byte(os.Getenv("SOCKET_DIR")), 0600); err != nil {
				panic(err)
			}
		}
		switch mode {
		case "--junit-helper-health":
			if _, err := os.Stat(os.Args[2]); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "--junit-helper-service", "--junit-helper-stubborn-service", "--junit-helper-never-healthy-service", "--junit-helper-crashing-service":
			shutdown := make(chan os.Signal, 1)
			signal.Notify(shutdown, syscall.SIGTERM)
			markReady(os.Args[2])
			fmt.Println("service stdout <&>")
			for {
				select {
				case <-shutdown:
					if mode != "--junit-helper-stubborn-service" {
						markReady(os.Args[3])
						os.Exit(0)
					}
				case <-time.After(10 * time.Millisecond):
					if mode == "--junit-helper-crashing-service" {
						if _, err := os.Stat(os.Args[4]); err == nil {
							fmt.Fprintln(os.Stderr, "service crash diagnostic")
							os.Exit(1)
						}
					}
				}
			}
		case "--junit-helper-held-pipe":
			markReady(os.Args[2])
			time.Sleep(30 * time.Second)
			os.Exit(0)
		case "--junit-helper-pipe-parent", "--junit-helper-leaked-pipe-test":
			fmt.Println("child stdout <&>")
			fmt.Fprintln(os.Stderr, "child stderr")
			executable, err := os.Executable()
			if err != nil {
				panic(err)
			}
			child := exec.Command(executable, "--junit-helper-held-pipe", os.Args[3])
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				panic(err)
			}
			for {
				if _, err := os.Stat(os.Args[3]); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			markReady(os.Args[2])
			if mode == "--junit-helper-pipe-parent" {
				os.Exit(0)
			}
			time.Sleep(30 * time.Second)
			os.Exit(0)
		case "--junit-helper-blocking-test":
			markReady(os.Args[2])
			time.Sleep(30 * time.Second)
			os.Exit(0)
		case "--junit-helper-unlink-capture", "--junit-helper-audit-capture", "--junit-helper-pass", "--junit-helper-child-xml", "--junit-helper-fail", "--junit-helper-misleading-fail":
			fmt.Println("child stdout <&>")
			fmt.Fprintln(os.Stderr, "child stderr")
			if mode == "--junit-helper-audit-capture" || mode == "--junit-helper-unlink-capture" {
				entries, err := os.ReadDir(os.Getenv("TEST_TMPDIR"))
				if err != nil {
					panic(err)
				}
				found := false
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".svcinit-output-") {
						found = true
						if mode == "--junit-helper-unlink-capture" {
							if err := os.Remove(filepath.Join(os.Getenv("TEST_TMPDIR"), entry.Name())); err != nil {
								panic(err)
							}
							fmt.Println("output after capture file removal")
						}
					}
				}
				if !found {
					panic("capture file is not in TEST_TMPDIR")
				}
			}
			if mode != "--junit-helper-pass" && mode != "--junit-helper-audit-capture" && mode != "--junit-helper-unlink-capture" {
				xml := `<testsuite name="child" tests="2" failures="0"><testcase name="child-a"/><testcase name="child-b"/></testsuite>`
				if mode == "--junit-helper-fail" {
					// Exercise nested suites, declarations, and detailed failures.
					xml = `<?xml version="1.0"?><testsuites><testsuite name="child" tests="2" failures="1"><testcase name="child-a"/><testcase name="child-b"><failure message="child assertion">expected &lt;value&gt;</failure></testcase></testsuite></testsuites>`
				}
				if err := os.WriteFile(os.Getenv("XML_OUTPUT_FILE"), []byte(xml), 0600); err != nil {
					panic(err)
				}
			}
			if mode == "--junit-helper-fail" || mode == "--junit-helper-misleading-fail" {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func stopHelper(t *testing.T, pidPath string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			t.Error(err)
			return
		}
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
		}
	})
}

type reportCase struct {
	Name    string `xml:"name,attr"`
	Failure *struct {
		Text string `xml:",chardata"`
	} `xml:"failure"`
}

type reportSuite struct {
	Tests     int          `xml:"tests,attr"`
	Failures  int          `xml:"failures,attr"`
	Case      []reportCase `xml:"testcase"`
	SystemOut string       `xml:"system-out"`
}

func TestServiceRunnerFinalJUnit(t *testing.T) {
	runner, err := runfiles.Rlocation(*svcinitRunfile)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, child, service, failure, trigger string
		tests                                  int
		noXML, badCaptureDir                   bool
	}{
		{name: "success", child: "pass", tests: 1},
		{name: "capture_in_test_tmpdir", child: "audit-capture", tests: 1},
		{name: "capture_file_removed", child: "unlink-capture", tests: 1},
		{name: "capture_unavailable", child: "pass", tests: 1, badCaptureDir: true},
		{name: "leaked_descendant_after_pass", child: "pipe-parent", tests: 1},
		{name: "child_report", child: "child-xml", tests: 2},
		{name: "child_failure", child: "fail", failure: "exit status 1", tests: 3},
		{name: "child_failure_with_passing_report", child: "misleading-fail", failure: "exit status 1", tests: 3},
		{name: "startup_failure", child: "pass", service: "missing", failure: "no such file", tests: 1},
		{name: "graceful_shutdown", child: "pass", service: "service", tests: 1},
		{name: "shutdown_failure_after_child_pass", child: "child-xml", service: "stubborn-service", failure: "did not handle SIGTERM", tests: 3},
		{name: "child_and_shutdown_failure", child: "fail", service: "stubborn-service", failure: "did not handle SIGTERM", tests: 3},
		{name: "interrupt_during_test", child: "blocking-test", failure: "Interrupted by", trigger: "interrupt", tests: 1},
		{name: "interrupt_without_xml", child: "blocking-test", failure: "canceled", trigger: "interrupt", noXML: true},
		{name: "interrupt_during_startup", child: "pass", service: "never-healthy-service", failure: "Interrupted by", trigger: "startup-interrupt", tests: 1},
		{name: "startup_interrupt_without_xml", child: "pass", service: "never-healthy-service", failure: "canceled", trigger: "startup-interrupt", noXML: true},
		{name: "service_crash_during_test", child: "blocking-test", service: "crashing-service", failure: "exited with error", trigger: "crash", tests: 1},
		{name: "second_interrupt", child: "leaked-pipe-test", failure: "Interrupted by", trigger: "second-interrupt", tests: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.child == "unlink-capture" && runtime.GOOS == "windows" {
				t.Skip("Windows does not allow unlinking this open file")
			}
			temporary := t.TempDir()
			xmlPath := filepath.Join(temporary, "reports", "test.xml")
			if err := os.Mkdir(filepath.Dir(xmlPath), 0700); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(temporary, "ready")
			stopped := filepath.Join(temporary, "stopped")
			childReady := filepath.Join(temporary, "child-ready")
			leakedPID := filepath.Join(temporary, "leaked-pid")
			crash := filepath.Join(temporary, "crash")
			stopHelper(t, leakedPID)
			specs := map[string]any{}
			if scenario.service != "" {
				executable, healthReady := helper, ready
				if scenario.service == "missing" {
					executable = filepath.Join(temporary, "missing-service")
				}
				if scenario.service == "never-healthy-service" {
					healthReady = filepath.Join(temporary, "never-ready")
				}
				specs["//fixture:service"] = map[string]any{
					"label": "//fixture:service", "type": "service", "exe": executable,
					"env":          map[string]string{},
					"args":         []string{"--junit-helper-" + scenario.service, ready, stopped, crash},
					"health_check": helper, "health_check_label": "//fixture:health",
					"health_check_args":     []string{"--junit-helper-health", healthReady},
					"health_check_interval": "10ms", "health_check_timeout": "2s",
					"expected_start_duration": "2s", "shutdown_signal": "SIGTERM",
					"shutdown_timeout": "100ms", "enforce_graceful_shutdown": true,
				}
			}
			encoded, err := json.Marshal(specs)
			if err != nil {
				t.Fatal(err)
			}
			specPath := filepath.Join(temporary, "specs.json")
			envPath := filepath.Join(temporary, "env.json")
			for path, contents := range map[string][]byte{specPath: encoded, envPath: []byte("{}")} {
				if err := os.WriteFile(path, contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, runner, "--junit-helper-"+scenario.child, childReady, leakedPID)
			// Bound pipe reads even if a surviving descendant holds stdout open.
			cmd.WaitDelay = time.Second
			cmd.Env = append(os.Environ(),
				"JUNIT_SOCKET_DIR_RECORD="+filepath.Join(temporary, "socket-dir"),
				"TEST_TARGET=//fixture:"+scenario.name,
				"TEST_TMPDIR="+temporary,
				"XML_OUTPUT_FILE="+xmlPath,
				"IBAZEL_NOTIFY_CHANGES=",
				"TEST_TOTAL_SHARDS=3", "TEST_SHARD_INDEX=1",
				"TEST_SHARD_STATUS_FILE="+filepath.Join(temporary, "shard-status"),
				"SVCINIT_KEEP_SERVICES_UP=False",
				"SVCINIT_SERVICE_SPECS_RLOCATION_PATH="+specPath,
				"SVCINIT_TEST_RLOCATION_PATH="+helper,
				"SVCINIT_TEST_ENV_RLOCATION_PATH="+envPath,
			)
			if scenario.noXML {
				cmd.Env = append(cmd.Env, "XML_OUTPUT_FILE=")
			}
			if scenario.badCaptureDir {
				cmd.Env = append(cmd.Env, "TEST_TMPDIR="+filepath.Join(temporary, "unavailable"))
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var runErr error
			done := make(chan struct{})
			go func() { runErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			waitFor := func(path string) {
				t.Helper()
				for {
					if _, err := os.Stat(path); err == nil {
						return
					}
					select {
					case <-done:
						t.Fatalf("runner exited before fixture became ready: %v\n%s", runErr, output.String())
					case <-ctx.Done():
						t.Fatal("fixture timed out before becoming ready")
					case <-time.After(time.Millisecond):
					}
				}
			}
			if scenario.trigger != "" {
				marker := childReady
				if scenario.trigger == "startup-interrupt" {
					marker = ready
				}
				waitFor(marker)
				if scenario.trigger == "crash" {
					markReady(crash)
				} else {
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
					if scenario.trigger == "second-interrupt" {
						// The descendant holds the capture pipe open during finalization.
						time.Sleep(100 * time.Millisecond)
						if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			<-done
			if ctx.Err() != nil {
				t.Fatalf("runner timed out: %s", output.String())
			}
			if (runErr != nil) != (scenario.failure != "") {
				t.Fatalf("unexpected runner result %v: %s", runErr, output.String())
			}
			if scenario.trigger == "interrupt" || scenario.trigger == "startup-interrupt" {
				socketDir, err := os.ReadFile(filepath.Join(temporary, "socket-dir"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(string(socketDir)); !os.IsNotExist(err) {
					t.Fatalf("interrupted run left its socket directory behind: %s", socketDir)
				}
			}
			if scenario.noXML {
				if !strings.Contains(output.String(), "Shutdown requested") {
					t.Fatalf("XML-disabled run did not handle the requested interruption: %s", output.String())
				}
				if _, err := os.Stat(xmlPath); !os.IsNotExist(err) {
					t.Fatal("XML-disabled run unexpectedly wrote a report")
				}
				return
			}
			contents, err := os.ReadFile(xmlPath)
			if err != nil {
				t.Fatalf("runner did not emit XML: %v\n%s", err, output.String())
			}
			var root struct {
				XMLName xml.Name
				reportSuite
				Suites []reportSuite `xml:"testsuite"`
			}
			if err := xml.Unmarshal(contents, &root); err != nil {
				t.Fatalf("invalid XML: %v\n%s", err, contents)
			}
			suites := root.Suites
			if root.XMLName.Local == "testsuite" {
				suites = []reportSuite{root.reportSuite}
			}
			cases := 0
			for _, suite := range suites {
				cases += len(suite.Case)
			}
			if cases != scenario.tests {
				t.Fatalf("unexpected case count: %s", contents)
			}
			final := suites[len(suites)-1]
			if scenario.failure != "" {
				if final.Failures != 1 || final.Case[0].Failure == nil || !strings.Contains(final.Case[0].Failure.Text, scenario.failure) {
					t.Fatalf("runner failure was not reported: %s\n%s", contents, output.String())
				}
			} else if final.Failures != 0 {
				t.Fatalf("successful runner reported failure: %s", contents)
			}
			if scenario.child == "fail" && (!strings.Contains(string(contents), "child assertion") || !strings.Contains(string(contents), "expected &lt;value&gt;") || !strings.Contains(final.Case[0].Failure.Text, "exit status 1")) {
				t.Fatalf("lost detailed child failure or test error during shutdown: %s", contents)
			}
			if scenario.name != "child_report" {
				if final.Case[0].Name != "//fixture:"+scenario.name+"_shard_2/3" {
					t.Fatalf("shard identity was lost: %s", contents)
				}
				if scenario.trigger == "" && scenario.service != "missing" && !scenario.badCaptureDir && (!strings.Contains(final.SystemOut, "child stdout <&>") || !strings.Contains(final.SystemOut, "child stderr")) {
					t.Fatalf("child output was lost: %s", contents)
				}
				if scenario.service == "service" && (!strings.Contains(final.SystemOut, "service stdout <&>") || !strings.Contains(final.SystemOut, "Stopping")) {
					t.Fatalf("service output or shutdown diagnostics were lost: %s", contents)
				}
			}
			if scenario.child == "unlink-capture" && !strings.Contains(final.SystemOut, "output after capture file removal") {
				t.Fatal("removing the capture pathname lost output or changed the test result")
			}
			if scenario.badCaptureDir && !strings.Contains(final.SystemOut, "Unable to capture") {
				t.Fatalf("capture setup failure was not reported: %s", contents)
			}
			if scenario.name == "leaked_descendant_after_pass" && !strings.Contains(final.SystemOut, "drain deadline") {
				t.Fatalf("capture truncation was not reported: %s", contents)
			}
			if scenario.trigger == "second-interrupt" && !strings.Contains(output.String(), "Multiple Ctrl-C detected") {
				t.Fatalf("forced-exit path was not exercised: %s", output.String())
			}
			if scenario.service == "service" {
				if _, err := os.Stat(stopped); err != nil {
					t.Fatal("runner returned before graceful service shutdown completed")
				}
			}
		})
	}
}

func TestWaitDelayBoundsInheritedOutputPipe(t *testing.T) {
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	pidPath := filepath.Join(temporary, "held-pipe-pid")
	stopHelper(t, pidPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "--junit-helper-pipe-parent", filepath.Join(temporary, "ready"), pidPath)
	cmd.WaitDelay = 100 * time.Millisecond
	start := time.Now()
	output, err := cmd.CombinedOutput()
	if !errors.Is(err, exec.ErrWaitDelay) || time.Since(start) > time.Second {
		t.Fatalf("inherited output pipe did not have a bounded wait: %v\n%s", err, output)
	}
}
