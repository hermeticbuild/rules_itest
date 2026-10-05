package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readJUnit(t *testing.T, path string) junitSuite {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
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
			if err := writeJUnitReport(path, target, 1250*time.Millisecond, exitCode, message); err != nil {
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
	if err := writeJUnitReport(path, "//service:test", time.Second, 0, ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(child) {
		t.Fatal("successful runner replaced the detailed child report")
	}
	if err := writeJUnitReport(path, "//service:test", time.Second, 1, "shutdown failed after child passed"); err != nil {
		t.Fatal(err)
	}
	if report := readJUnit(t, path); report.Failures != 1 || report.Case.Failure == nil {
		t.Fatalf("shutdown failure was masked by child XML: %+v", report)
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
	if report := readJUnit(t, path); report.Failures != 1 || report.Case.Failure.Text != "startup or shutdown failed" {
		t.Fatalf("incorrect panic report: %+v", report)
	}
}

func TestJUnitReporterIsOneShotOnly(t *testing.T) {
	t.Setenv("TEST_TARGET", "//service:test")
	t.Setenv("XML_OUTPUT_FILE", filepath.Join(t.TempDir(), "test.xml"))
	t.Setenv("IBAZEL_NOTIFY_CHANGES", "")
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
	if err := writeJUnitReport(path, "//service:test", time.Second, 0, ""); err == nil {
		t.Fatal("XML write failure was ignored")
	}
}
