package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func readJUnit(t *testing.T, path string) junitSuite {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		XMLName xml.Name
		Suites  []junitSuite `xml:"testsuite"`
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if root.XMLName.Local == "testsuites" {
		return root.Suites[len(root.Suites)-1]
	}
	var report junitSuite
	if err := xml.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestJUnitFinalResult(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(string(rune('0'+exitCode)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.xml")
			target := "//service:unicode_日本_<&\""
			message := "shutdown failed: <error> & \"details\""
			if err := writeJUnitReport(path, target, 1250*time.Millisecond, exitCode, message, ""); err != nil {
				t.Fatal(err)
			}
			report := readJUnit(t, path)
			if report.Name != target || report.Case.Name != target || report.Tests != 1 || report.Failures != exitCode || report.Time != "1.250000" {
				t.Fatalf("incorrect final report: %+v", report)
			}
			if exitCode == 1 && (report.Case.Failure == nil || report.Case.Failure.Text != message) {
				t.Fatalf("failure details did not round trip: %+v", report.Case.Failure)
			}
			if exitCode == 0 && report.Case.Failure != nil {
				t.Fatal("success report contains a failure")
			}
		})
	}
}

func TestJUnitPreservesChildReportUntilRunnerFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.xml")
	child := []byte("<testsuite tests=\"2\"><testcase name=\"child-a\"/><testcase name=\"child-b\"/></testsuite>")
	if err := os.WriteFile(path, child, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJUnitReport(path, "//service:test", time.Second, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(child) {
		t.Fatal("successful runner replaced the detailed child report")
	}
	if err := writeJUnitReport(path, "//service:test", time.Second, 1, "shutdown failed after child passed", ""); err != nil {
		t.Fatal(err)
	}
	if report := readJUnit(t, path); report.Failures != 1 || report.Case.Failure == nil {
		t.Fatalf("shutdown failure was masked by child XML: %+v", report)
	}
	merged, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(merged), string(child)) {
		t.Fatal("runner failure lost the child cases")
	}
}

func TestJUnitPanicWritesFailureAndPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.xml")
	reporter := &junitReporter{path: path, target: "//service:test", start: time.Now()}
	func() {
		defer func() {
			if got := recover(); got != "startup or shutdown failed" {
				t.Fatalf("runner panic was swallowed: %v", got)
			}
		}()
		defer reporter.finishOnReturn()
		panic("startup or shutdown failed")
	}()
	if report := readJUnit(t, path); report.Failures != 1 || !strings.Contains(report.Case.Failure.Text, "startup or shutdown failed") || !strings.Contains(report.Case.Failure.Text, "TestJUnitPanicWritesFailureAndPropagates") {
		t.Fatalf("incorrect panic report: %+v", report)
	}
}

func TestJUnitReporterIsOneShotOnly(t *testing.T) {
	t.Setenv("TEST_TARGET", "//service:test")
	t.Setenv("XML_OUTPUT_FILE", filepath.Join(t.TempDir(), "test.xml"))
	t.Setenv("IBAZEL_NOTIFY_CHANGES", "")
	t.Setenv("TEST_TOTAL_SHARDS", "")
	t.Setenv("TEST_SHARD_INDEX", "")
	if newJUnitReporter(time.Now()) == nil {
		t.Fatal("one-shot service test needs XML")
	}
	t.Setenv("IBAZEL_NOTIFY_CHANGES", "y")
	if newJUnitReporter(time.Now()) != nil {
		t.Fatal("interactive reload should not emit one-shot XML")
	}
	t.Setenv("IBAZEL_NOTIFY_CHANGES", "")
	t.Setenv("TEST_TARGET", "")
	if newJUnitReporter(time.Now()) != nil {
		t.Fatal("bazel run should not emit test XML")
	}
	t.Setenv("TEST_TARGET", "//service:test")
	t.Setenv("XML_OUTPUT_FILE", "")
	if newJUnitReporter(time.Now()) != nil {
		t.Fatal("missing XML_OUTPUT_FILE should disable reporting")
	}
}

func TestJUnitWriteErrorIsReturned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "test.xml")
	if err := writeJUnitReport(path, "//service:test", time.Second, 0, "", ""); err == nil {
		t.Fatal("XML write failure was ignored")
	}
}

func TestJUnitMergesDetailedFailures(t *testing.T) {
	for _, child := range []string{
		`<testsuite name="child" tests="2" failures="1"><testcase name="pass"/><testcase name="fail"><failure message="assertion">details &amp; more</failure></testcase></testsuite>`,
		`<?xml version="1.0"?><testsuites tests="2" errors="1"><testsuite name="child" tests="2" errors="1"><testcase name="pass"/><testcase name="error"><error message="exception"><![CDATA[stack <&>]]></error></testcase><system-out>child log</system-out></testsuite></testsuites>`,
	} {
		path := filepath.Join(t.TempDir(), "test.xml")
		if err := os.WriteFile(path, []byte(child), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeJUnitReport(path, "//service:test", time.Second, 1, "test and shutdown failed", "runner log"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		suites, err := childJUnitSuites([]byte(child))
		if err != nil || !strings.Contains(string(data), suites) {
			t.Fatalf("lost child details: %s", data)
		}
		if report := readJUnit(t, path); report.Failures != 1 || report.SystemOut != "runner log" {
			t.Fatalf("missing runner failure or output: %+v", report)
		}
	}
}

func TestJUnitInvalidChildDoesNotMaskFailure(t *testing.T) {
	for _, child := range []string{"", "<testsuite>", "<wrong/>", "<testsuite/><testsuite/>"} {
		path := filepath.Join(t.TempDir(), "test.xml")
		if err := os.WriteFile(path, []byte(child), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeJUnitReport(path, "//service:test", time.Second, 1, "failed", ""); err != nil {
			t.Fatal(err)
		}
		report := readJUnit(t, path)
		if report.Failures != 1 || !strings.Contains(report.SystemOut, child) || !strings.Contains(report.SystemOut, "Invalid child JUnit report") {
			t.Fatalf("invalid child hid the failure or was discarded: %+v", report)
		}
	}
}

func TestJUnitOutputAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.xml")
	output := "stdout <&>\nstderr\n\x1b[31mservice output\x1b[0m"
	if err := writeJUnitReport(path, "//service:test", time.Second, 1, "\x1b[31mfailed\x1b[0m", output); err != nil {
		t.Fatal(err)
	}
	report := readJUnit(t, path)
	if report.SystemOut != "stdout <&>\nstderr\nservice output" || report.Case.Failure.Text != "failed" {
		t.Fatalf("output was not preserved or cleaned: %+v", report)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0644 {
			t.Fatalf("report is not readable by output collectors: %v, %v", info, err)
		}
	}
}

func TestJUnitShardNames(t *testing.T) {
	t.Setenv("TEST_TARGET", "//service:test")
	t.Setenv("XML_OUTPUT_FILE", filepath.Join(t.TempDir(), "test.xml"))
	t.Setenv("IBAZEL_NOTIFY_CHANGES", "")
	t.Setenv("TEST_TOTAL_SHARDS", "3")
	t.Setenv("TEST_SHARD_INDEX", "1")
	if got := newJUnitReporter(time.Now()).target; got != "//service:test_shard_2/3" {
		t.Fatalf("shards have no unique identity: %s", got)
	}
}

func TestJUnitRetainsMultipleFailureCauses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.xml")
	r := &junitReporter{path: path, target: "//service:test", start: time.Now()}
	r.recordFailure("Test failed: exit status 1")
	if err := r.finish(2, "shutdown failed"); err != nil {
		t.Fatal(err)
	}
	message := readJUnit(t, path).Case.Failure.Text
	if !strings.Contains(message, "exit status 1") || !strings.Contains(message, "shutdown failed") {
		t.Fatalf("cleanup hid the original failure: %s", message)
	}
}
