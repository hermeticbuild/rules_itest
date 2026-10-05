package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The service runner owns the final result, including startup and shutdown.
type junitReporter struct {
	path           string
	target         string
	start          time.Time
	once           sync.Once
	err            error
	exitCode       int
	capture        *junitOutput
	mu             sync.Mutex
	success        bool
	causes         []string
	captureWarning string
}

func newJUnitReporter(start time.Time) *junitReporter {
	path, target := os.Getenv("XML_OUTPUT_FILE"), os.Getenv("TEST_TARGET")
	if path == "" || target == "" || os.Getenv("IBAZEL_NOTIFY_CHANGES") == "y" || shouldKeepServicesUp {
		return nil
	}
	if total, err := strconv.Atoi(os.Getenv("TEST_TOTAL_SHARDS")); err == nil && total > 0 {
		if index, err := strconv.Atoi(os.Getenv("TEST_SHARD_INDEX")); err == nil && index >= 0 && index < total {
			target += fmt.Sprintf("_shard_%d/%d", index+1, total)
		}
	}
	return &junitReporter{path: path, target: target, start: start}
}

func (r *junitReporter) recordFailure(message string) {
	if r == nil || message == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cause := range r.causes {
		if cause == message {
			return
		}
	}
	r.causes = append(r.causes, message)
}

func (r *junitReporter) markSuccess() {
	if r != nil {
		r.mu.Lock()
		r.success = true
		r.mu.Unlock()
	}
}

// Defer this before service setup so it runs after cleanup on return or panic.
func (r *junitReporter) finishOnReturn() {
	if r == nil {
		return
	}
	if value := recover(); value != nil {
		if err := r.finish(2, fmt.Sprintf("%v\n%s", value, debug.Stack())); err != nil {
			log.Printf("Writing service-test XML: %v", err)
		}
		panic(value)
	}
	r.mu.Lock()
	success := r.success && len(r.causes) == 0
	r.mu.Unlock()
	exitCode, message := 0, ""
	if !success {
		exitCode, message = 1, "Service-test runner returned before successful test completion"
	}
	if err := r.finish(exitCode, message); err != nil {
		log.Printf("Writing service-test XML: %v", err)
		exitCode = 1
	}
	if r.exitCode != 0 {
		os.Exit(r.exitCode)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
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
		r.recordFailure(message)
		r.mu.Lock()
		message = strings.Join(r.causes, "\n")
		if len(r.causes) != 0 && exitCode == 0 {
			exitCode = 1
		}
		r.mu.Unlock()
		output, err := r.capture.finish()
		if r.captureWarning != "" {
			output += "\n" + r.captureWarning
		}
		if err != nil {
			exitCode = 1
			message += fmt.Sprintf("\nCapturing test output: %v", err)
		}
		r.exitCode = exitCode
		r.err = writeJUnitReport(r.path, r.target, time.Since(r.start), exitCode, message, output)
		if r.err == nil {
			r.err = err
		}
	})
	return r.err
}

type junitSuite struct {
	XMLName   xml.Name  `xml:"testsuite"`
	Name      string    `xml:"name,attr"`
	Tests     int       `xml:"tests,attr"`
	Failures  int       `xml:"failures,attr"`
	Time      string    `xml:"time,attr"`
	Case      junitCase `xml:"testcase"`
	SystemOut string    `xml:"system-out"`
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

// Retain the child's suites verbatim, including case details and extensions.
// Strip only an outer testsuites wrapper and the XML declaration for merging.
func childJUnitSuites(data []byte) (string, []xml.Attr, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var suites string
	var namespaces []xml.Attr
	found := false
	for {
		offset := decoder.InputOffset()
		token, err := decoder.Token()
		if err == io.EOF {
			if !found {
				return "", nil, fmt.Errorf("missing JUnit suite")
			}
			return suites, namespaces, nil
		}
		if err != nil {
			return "", nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if found || (token.Name.Local != "testsuite" && token.Name.Local != "testsuites") {
				return "", nil, fmt.Errorf("unexpected JUnit root %s", token.Name.Local)
			}
			for _, attr := range token.Attr {
				switch {
				case attr.Name.Space == "xmlns":
					attr.Name = xml.Name{Local: "xmlns:" + attr.Name.Local}
				case attr.Name.Space == "" && attr.Name.Local == "xmlns":
				case attr.Name.Space == "http://www.w3.org/XML/1998/namespace":
					attr.Name = xml.Name{Local: "xml:" + attr.Name.Local}
				default:
					continue
				}
				namespaces = append(namespaces, attr)
			}
			var root struct {
				Inner string `xml:",innerxml"`
			}
			if err := decoder.DecodeElement(&root, &token); err != nil {
				return "", nil, err
			}
			suites = root.Inner
			if token.Name.Local == "testsuite" {
				suites = string(data[offset:decoder.InputOffset()])
			}
			found = true
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return "", nil, fmt.Errorf("text outside JUnit suite")
			}
		}
	}
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func writeJUnitReport(path, target string, duration time.Duration, exitCode int, message, output string) error {
	child, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && exitCode == 0 {
		if err := os.Chmod(path, 0644); err != nil {
			log.Printf("Setting child JUnit report permissions: %v", err)
		}
		return nil
	}
	var suites string
	var namespaces []xml.Attr
	if err == nil {
		suites, namespaces, err = childJUnitSuites(child)
		if err != nil {
			// Invalid child XML must not mask the runner's failure. Keep its
			// contents available as diagnostics in the replacement report.
			output += fmt.Sprintf("\nInvalid child JUnit report (%v):\n%s", err, child)
		}
	}
	elapsed := fmt.Sprintf("%.6f", duration.Seconds())
	report := junitSuite{
		Name: target, Tests: 1, Time: elapsed,
		Case:      junitCase{Name: target, Time: elapsed},
		SystemOut: ansiEscape.ReplaceAllString(output, ""),
	}
	if exitCode != 0 {
		report.Failures = 1
		report.Case.Failure = &junitFailure{
			Message: fmt.Sprintf("service-test runner exited with code %d", exitCode),
			Text:    ansiEscape.ReplaceAllString(message, ""),
		}
	}
	encoded, err := xml.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if suites != "" {
		var wrapper bytes.Buffer
		encoder := xml.NewEncoder(&wrapper)
		if err := encoder.EncodeToken(xml.StartElement{Name: xml.Name{Local: "testsuites"}, Attr: namespaces}); err != nil {
			return err
		}
		if err := encoder.Flush(); err != nil {
			return err
		}
		encoded = []byte(wrapper.String() + "\n" + suites + "\n" + string(encoded) + "\n</testsuites>")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".svcinit-junit-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0644); err != nil {
		return err
	}
	if _, err := file.WriteString(xml.Header); err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// Capture runner and descendant output while still streaming it to Bazel.
// An on-disk buffer avoids retaining logs while services run.
type junitOutput struct {
	stdout, stderr *os.File
	reader, writer *os.File
	file           *os.File
	done           chan error
	output         *junitOutputWriter
}

func (r *junitReporter) captureOutput() error {
	if r == nil {
		return nil
	}
	file, err := os.CreateTemp(os.Getenv("TEST_TMPDIR"), ".svcinit-output-*")
	if err != nil {
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		return err
	}
	capture := &junitOutput{stdout: os.Stdout, stderr: os.Stderr, reader: reader, writer: writer, file: file, done: make(chan error, 1)}
	capture.output = &junitOutputWriter{file: file, stdout: capture.stdout}
	r.capture = capture
	os.Stdout, os.Stderr = writer, writer
	log.SetOutput(writer)
	go func() {
		_, err := io.Copy(capture.output, reader)
		capture.done <- err
	}()
	return nil
}

func (c *junitOutput) finish() (string, error) {
	if c == nil {
		return "", nil
	}
	log.SetOutput(c.stderr)
	c.writer.Close()
	// A forcibly terminated runner can still have descendants holding the
	// pipe open. Bound draining so writing the failure report cannot hang.
	var captureErr error
	var warning string
	select {
	case captureErr = <-c.done:
	case <-time.After(time.Second):
		c.reader.Close()
		warning = "\nOutput capture stopped after the drain deadline; a descendant may still hold the output pipe open.\n"
	}
	c.reader.Close()
	captureErr = errors.Join(captureErr, c.output.failure())
	c.file.Close()
	defer os.Remove(c.file.Name())
	data, err := os.ReadFile(c.file.Name())
	if err != nil {
		return "", err
	}
	return string(data) + warning, captureErr
}

// A broken log destination must not stop draining the child output pipe.
// Disable that destination, retain its error, and keep forwarding to the other.
type junitOutputWriter struct {
	file, stdout io.Writer
	mu           sync.Mutex
	err          error
}

func (w *junitOutputWriter) Write(data []byte) (int, error) {
	for _, destination := range []struct {
		name   string
		writer *io.Writer
	}{
		{name: "capture file", writer: &w.file},
		{name: "test output", writer: &w.stdout},
	} {
		if *destination.writer == nil {
			continue
		}
		n, err := (*destination.writer).Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.mu.Lock()
			w.err = errors.Join(w.err, fmt.Errorf("writing %s: %w", destination.name, err))
			w.mu.Unlock()
			*destination.writer = nil
		}
	}
	return len(data), nil
}

func (w *junitOutputWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
