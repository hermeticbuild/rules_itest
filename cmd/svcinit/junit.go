package main

import (
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The service runner owns the final result, including startup and shutdown.
// A child test's successful XML alone cannot attest that services stopped cleanly.
type junitReporter struct {
	path   string
	target string
	start  time.Time
	once   sync.Once
	err    error
}

func newJUnitReporter(start time.Time) *junitReporter {
	path, target := os.Getenv("XML_OUTPUT_FILE"), os.Getenv("TEST_TARGET")
	if path == "" || target == "" || os.Getenv("IBAZEL_NOTIFY_CHANGES") == "y" || shouldKeepServicesUp {
		return nil
	}
	return &junitReporter{path: path, target: target, start: start}
}

// Defer this before service setup so it runs after cleanup on normal return or panic.
func (r *junitReporter) finishOnReturn() {
	if r == nil {
		return
	}
	if value := recover(); value != nil {
		if err := r.finish(2, fmt.Sprint(value)); err != nil {
			log.Printf("Writing service-test XML: %v", err)
		}
		panic(value)
	}
	if err := r.finish(0, ""); err != nil {
		log.Printf("Writing service-test XML: %v", err)
		os.Exit(1)
	}
}

func (r *junitReporter) exitFailure(message string) {
	log.Print(message)
	if err := r.finish(1, message); err != nil {
		log.Printf("Writing service-test XML: %v", err)
	}
	os.Exit(1)
}

func (r *junitReporter) finish(exitCode int, message string) error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.err = writeJUnitReport(r.path, r.target, time.Since(r.start), exitCode, message)
	})
	return r.err
}

type junitSuite struct {
	XMLName  xml.Name  `xml:"testsuite"`
	Name     string    `xml:"name,attr"`
	Tests    int       `xml:"tests,attr"`
	Failures int       `xml:"failures,attr"`
	Time     string    `xml:"time,attr"`
	Case     junitCase `xml:"testcase"`
}

type junitCase struct {
	Name    string        `xml:"name,attr"`
	Time    string        `xml:"time,attr"`
	Failure *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func writeJUnitReport(path, target string, duration time.Duration, exitCode int, message string) error {
	// Preserve detailed reports from successful child tests. A runner failure
	// overrides child XML because it may have been written before shutdown.
	if exitCode == 0 {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	elapsed := fmt.Sprintf("%.6f", duration.Seconds())
	report := junitSuite{Name: target, Tests: 1, Time: elapsed, Case: junitCase{Name: target, Time: elapsed}}
	if exitCode != 0 {
		report.Failures = 1
		report.Case.Failure = &junitFailure{Message: fmt.Sprintf("service-test runner exited with code %d", exitCode), Text: message}
	}
	output, err := os.CreateTemp(filepath.Dir(path), ".svcinit-junit-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	if _, err := output.WriteString(xml.Header); err != nil {
		return err
	}
	encoder := xml.NewEncoder(output)
	encoder.Indent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(output.Name(), path)
}
