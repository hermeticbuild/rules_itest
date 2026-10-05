package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

var svcinitRunfile = flag.String("svcinit", "", "Service runner runfile")

// Reuse this test binary as the probe and service so the integration fixtures
// need no shell, host runtime or network service beyond svcinit itself.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--junit-helper-health":
			if _, err := os.Stat(os.Args[2]); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "--junit-helper-service", "--junit-helper-stubborn-service":
			shutdown := make(chan os.Signal, 1)
			signal.Notify(shutdown, syscall.SIGTERM)
			if err := os.WriteFile(os.Args[2], []byte("ready"), 0600); err != nil {
				panic(err)
			}
			for {
				<-shutdown
				if os.Args[1] == "--junit-helper-service" {
					if err := os.WriteFile(os.Args[3], []byte("stopped"), 0600); err != nil {
						panic(err)
					}
					os.Exit(0)
				}
			}
		case "--junit-helper-pass":
			os.Exit(0)
		case "--junit-helper-child-xml", "--junit-helper-fail":
			if err := os.WriteFile(os.Getenv("XML_OUTPUT_FILE"), []byte("<testsuite tests=\"2\" failures=\"0\"><testcase name=\"child-a\"/><testcase name=\"child-b\"/></testsuite>"), 0600); err != nil {
				panic(err)
			}
			if os.Args[1] == "--junit-helper-fail" {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
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
		name, child, service, failure string
		tests                         int
	}{
		{name: "success", child: "pass", tests: 1},
		{name: "child_report", child: "child-xml", tests: 2},
		{name: "child_failure", child: "fail", failure: "exit status 1", tests: 1},
		{name: "startup_failure", child: "pass", service: "missing", failure: "no such file", tests: 1},
		{name: "graceful_shutdown", child: "pass", service: "service", tests: 1},
		{name: "shutdown_failure_after_child_pass", child: "child-xml", service: "stubborn-service", failure: "did not handle SIGTERM", tests: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			temporary := t.TempDir()
			xmlPath := filepath.Join(temporary, "test.xml")
			ready := filepath.Join(temporary, "ready")
			stopped := filepath.Join(temporary, "stopped")
			specs := map[string]any{}
			if scenario.service != "" {
				executable := helper
				if scenario.service == "missing" {
					executable = filepath.Join(temporary, "missing-service")
				}
				specs["//fixture:service"] = map[string]any{
					"label": "//fixture:service", "type": "service", "exe": executable,
					"env":          map[string]string{},
					"args":         []string{"--junit-helper-" + scenario.service, ready, stopped},
					"health_check": helper, "health_check_label": "//fixture:health",
					"health_check_args":     []string{"--junit-helper-health", ready},
					"health_check_interval": "10ms", "health_check_timeout": "1s",
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
			cmd := exec.CommandContext(ctx, runner, "--junit-helper-"+scenario.child)
			cmd.Env = append(os.Environ(),
				"TEST_TARGET=//fixture:"+scenario.name,
				"TEST_TMPDIR="+temporary,
				"XML_OUTPUT_FILE="+xmlPath,
				"IBAZEL_NOTIFY_CHANGES=",
				"SVCINIT_KEEP_SERVICES_UP=False",
				"SVCINIT_SERVICE_SPECS_RLOCATION_PATH="+specPath,
				"SVCINIT_TEST_RLOCATION_PATH="+helper,
				"SVCINIT_TEST_ENV_RLOCATION_PATH="+envPath,
			)
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("runner timed out: %s", output)
			}
			if (runErr != nil) != (scenario.failure != "") {
				t.Fatalf("unexpected runner result %v: %s", runErr, output)
			}
			contents, err := os.ReadFile(xmlPath)
			if err != nil {
				t.Fatalf("runner did not emit XML: %v\n%s", err, output)
			}
			var report struct {
				Tests    int `xml:"tests,attr"`
				Failures int `xml:"failures,attr"`
				Case     []struct {
					Failure *struct {
						Text string `xml:",chardata"`
					} `xml:"failure"`
				} `xml:"testcase"`
			}
			if err := xml.Unmarshal(contents, &report); err != nil {
				t.Fatalf("invalid XML: %v\n%s", err, contents)
			}
			if report.Tests != scenario.tests || len(report.Case) != scenario.tests {
				t.Fatalf("unexpected case count: %s", contents)
			}
			if scenario.failure != "" {
				if report.Failures != 1 || report.Case[0].Failure == nil || !strings.Contains(report.Case[0].Failure.Text, scenario.failure) {
					t.Fatalf("runner failure was not reported: %s\n%s", contents, output)
				}
			} else if report.Failures != 0 {
				t.Fatalf("successful runner reported failure: %s", contents)
			}
			if scenario.service == "service" {
				if _, err := os.Stat(stopped); err != nil {
					t.Fatal("runner returned before graceful service shutdown completed")
				}
			}
		})
	}
}
