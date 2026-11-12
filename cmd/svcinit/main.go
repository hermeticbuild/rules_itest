package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"rules_itest/logger"
	"rules_itest/runner"
	"rules_itest/runner/topological"
	"rules_itest/svcctl"
	"rules_itest/svclib"
)

// iBazel's stdin notifications are complete lines, including the final newline.
const (
	IBazelBuildStarted   = "IBAZEL_BUILD_STARTED\n"
	IBazelBuildFailed    = "IBAZEL_BUILD_COMPLETED FAILURE\n"
	IBazelBuildSucceeded = "IBAZEL_BUILD_COMPLETED SUCCESS\n"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var (
	terseOutput            = os.Getenv("SVCINIT_TERSE_OUTPUT") == "True"
	allowConfiguringTmpdir = os.Getenv("SVCINIT_ALLOW_CONFIGURING_TMPDIR") == "True"
	enablePerServiceReload = os.Getenv("SVCINIT_ENABLE_PER_SERVICE_RELOAD") == "True"
	shouldKeepServicesUp   = os.Getenv("SVCINIT_KEEP_SERVICES_UP") == "True"
)

// Assigned by x_def
var getAssignedPortRlocationPath string

func main() {
	start := time.Now()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	serviceSpecsPath, err := runfiles.Rlocation(os.Getenv("SVCINIT_SERVICE_SPECS_RLOCATION_PATH"))
	must(err)

	// Set up the environment properly so child processes can find their runfiles.
	runfilesEnv, err := runfiles.Env()
	must(err)
	for _, kv := range runfilesEnv {
		parts := strings.SplitN(kv, "=", 2)
		os.Setenv(parts[0], parts[1])
	}

	shouldHotReload := os.Getenv("IBAZEL_NOTIFY_CHANGES") == "y"
	testLabel := os.Getenv("TEST_TARGET")

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	builds := make(chan string)
	buildReadErr := make(chan error, 1)
	if shouldHotReload {
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Split(scanLinesKeepNewline)
			for scanner.Scan() {
				select {
				case builds <- scanner.Text():
				case <-ctx.Done():
					return
				}
			}
			// Clean EOF ends notifications, not the service group.
			if err := scanner.Err(); err != nil {
				buildReadErr <- err
			}
		}()
	}

	// Sockets have a short max path length (108 chars on Linux, 103 usable
	// chars on Darwin), so use /tmp instead of the long TEST_TMPDIR path.
	socketTempDir := ""
	if runtime.GOOS != "windows" {
		socketTempDir = "/tmp"
	}
	socketDir, err := os.MkdirTemp(socketTempDir, "")
	must(err)
	os.Setenv("SOCKET_DIR", socketDir)
	defer os.RemoveAll(socketDir)

	// If we are under `bazel run` for a service group, we may not have TEST_TMPDIR set.
	tmpDir := os.Getenv("TEST_TMPDIR")
	if tmpDir == "" {
		var err error
		tmpDir, err = os.MkdirTemp("", strings.ReplaceAll(testLabel, "/", "_"))
		must(err)
		defer os.RemoveAll(tmpDir)
	}
	os.Setenv("TEST_TMPDIR", tmpDir)

	if allowConfiguringTmpdir {
		// Leave the one that is already configured, unless we don't have one.
		if _, ok := os.LookupEnv("TMPDIR"); !ok {
			os.Setenv("TMPDIR", os.TempDir())
		}
	} else {
		// Typically it's better to match TEST_TMPDIR to ensure it's hermetic
		// and works the same way across `bazel run` and `bazel test`
		os.Setenv("TMPDIR", tmpDir)
	}

	getAssignedPortBinPath, err := runfiles.Rlocation(getAssignedPortRlocationPath)
	must(err)
	os.Setenv("GET_ASSIGNED_PORT_BIN", getAssignedPortBinPath)

	isOneShot := !shouldHotReload && testLabel != "" && !shouldKeepServicesUp

	unversionedSpecs, err := readServiceSpecs(serviceSpecsPath)
	must(err)

	ports, reservedPorts, listener, err := assignPorts(unversionedSpecs, "127.0.0.1:0")
	must(err)
	defer closeReservedPorts(reservedPorts)

	svcctlPort := listener.Addr().(*net.TCPAddr).Port
	svcctlPortStr := strconv.Itoa(svcctlPort)
	os.Setenv("SVCCTL_PORT", svcctlPortStr)

	if testLabel == "" {
		err = os.WriteFile("/tmp/svcctl_port", []byte(svcctlPortStr), 0600)
		must(err)
		defer os.Remove("/tmp/svcctl_port")
	}

	serviceSpecs, err := augmentServiceSpecs(unversionedSpecs, ports, svcctlPortStr)
	must(err)

	r, err := runner.New(ctx, serviceSpecs)
	must(err)

	mustStopAllForExit := func() map[string]*os.ProcessState {
		states, err := r.StopAll()
		cancelFunc()
		must(err)
		return states
	}

	servicesErrCh := make(chan error, len(unversionedSpecs))

	go func() {
		defer listener.Close()
		err := svcctl.Serve(ctx, listener, r, ports, servicesErrCh)
		if err != nil {
			log.Fatalf("svcctl.Serve: %v", err)
		}
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		count := 0
		for range signalCh {
			if count == 0 {
				log.Println("Shutdown requested, exiting gracefully. Press Ctrl-C again to force exit")
				cancelFunc()
				count++
			} else {
				log.Println("Multiple Ctrl-C detected, force-exiting")
				os.Exit(1)
			}
		}
	}()

	criticalPath, err := r.StartAll(servicesErrCh)
	if err != nil {
		mustStopAllForExit()
		if errors.Is(err, context.Canceled) {
			return
		}
	}
	must(err)

Runs:
	for {
		reportCriticalPath(criticalPath)
		var test testRun
		if testLabel != "" {
			test = startTest(ctx, ports, start)
		}
		fmt.Println()
		if shouldHotReload && !enablePerServiceReload {
			warnNoServiceReload(testLabel)
		}

		if isOneShot {
			err := waitOneShot(ctx, test.done, servicesErrCh)
			if errors.Is(err, context.Canceled) {
				break Runs
			}
			if err != nil {
				log.Print(err)
				mustStopAllForExit()
				os.Exit(1)
			}
			states := mustStopAllForExit()
			reportOneShot(testLabel, test, states)
			return
		}

		select {
		case <-ctx.Done():
			break Runs
		case err := <-buildReadErr:
			if test.done != nil {
				test.cancel()
				<-test.done
			}
			mustStopAllForExit()
			panic(fmt.Errorf("reading iBazel notifications: %w", err))
		case line := <-builds:
			os.Stderr.WriteString(line)
			unversionedSpecs, err := readServiceSpecs(serviceSpecsPath)
			must(err)
			serviceSpecs, err := augmentServiceSpecs(unversionedSpecs, ports, svcctlPortStr)
			must(err)
			if test.done != nil {
				test.cancel()
			}
			discardServiceErrors(servicesErrCh)
			criticalPath, err = r.UpdateSpecsAndRestart(serviceSpecs, servicesErrCh, []byte(line))
			must(err)
			continue
		case testErr := <-test.done:
			if testErr != nil {
				log.Printf("Encountered error during test run: %s\n", testErr)
			}
		case serviceErr := <-servicesErrCh:
			log.Print(serviceErr)
		}
		reportStartupTimes(testLabel, test, r.GetStartDurations())
	}
	log.Println("Shutting down services.")
	mustStopAllForExit()
	log.Println("Cleaning up.")
}

// scanLinesKeepNewline returns each line with its delimiter and rejects a partial final line.
func scanLinesKeepNewline(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF && len(data) != 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}

// A zero testRun has no child; its nil done channel is disabled in a select.
type testRun struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   <-chan error
}

func startTest(ctx context.Context, ports svclib.Ports, start time.Time) testRun {
	// Bazel's args attribute converts $ to $, so args arrive with
	// single-$ placeholders (e.g. ${@@//:svc}) unlike env/spec files
	// which preserve the literal $ since they're read from JSON.
	argReplacements := buildReplacements(ports, "${")
	testArgs := make([]string, len(os.Args[1:]))
	for i, arg := range os.Args[1:] {
		testArgs[i] = replaceAll(arg, argReplacements)
	}
	testPath, err := runfiles.Rlocation(os.Getenv("SVCINIT_TEST_RLOCATION_PATH"))
	must(err)

	testEnv, err := buildTestEnv(ports)
	must(err)

	fmt.Println("")
	if !terseOutput {
		log.Printf("Executing test: %s, %s\n", testPath, strings.Join(testArgs, " "))
	}
	testStartTime := time.Now()

	testCtx, cancel := context.WithCancel(ctx)
	testCmd := exec.CommandContext(testCtx, testPath, testArgs...)
	testCmd.Env = testEnv

	// Adjust remaining timeout to account for service startup.
	timeout := os.Getenv("TEST_TIMEOUT")
	if timeout != "" {
		timeoutVal, err := strconv.Atoi(timeout)
		if err != nil {
			fmt.Println(err)
		} else {
			timeoutVal -= int(math.Ceil(testStartTime.Sub(start).Seconds()))
			testCmd.Env = append(testCmd.Env, "TEST_TIMEOUT="+strconv.Itoa(timeoutVal))
		}
	}

	testCmd.Stdout = os.Stdout
	testCmd.Stderr = os.Stderr

	if err := testCmd.Start(); err != nil {
		panic(err)
	}

	done := make(chan error, 1)
	go func() {
		err := testCmd.Wait()
		cancel()
		done <- err
		log.Printf("Test duration: %s\n", time.Since(testStartTime))
	}()
	return testRun{cmd: testCmd, cancel: cancel, done: done}
}

func waitOneShot(ctx context.Context, testDone, serviceErrors <-chan error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serviceErrors:
		return fmt.Errorf("service failed: %w", err)
	case err := <-testDone:
		if err != nil {
			return err
		}
		// Both channels can be ready. A successful child must not hide a service failure.
		select {
		case err := <-serviceErrors:
			return fmt.Errorf("service failed: %w", err)
		default:
			return nil
		}
	}
}

func reportTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 8, 8, ' ', 0)
}

func reportCriticalPath(tasks []topological.Task) {
	w := reportTable()
	fmt.Fprintln(w, "\nTarget\tCritical Path Contribution")
	for _, task := range tasks {
		fmt.Fprintf(w, "%s\t%s\n", task.Key(), task.Duration())
	}
	must(w.Flush())
}

func reportOneShot(label string, test testRun, services map[string]*os.ProcessState) {
	w := reportTable()
	fmt.Fprintln(w, "Target\tUser Time\tSystem Time")
	for label, state := range services {
		fmt.Fprintf(w, "%s\t%s\t%s\n", label, state.UserTime(), state.SystemTime())
	}
	test.finishReport(w, label)
}

func reportStartupTimes(label string, test testRun, services map[string]time.Duration) {
	w := reportTable()
	fmt.Fprintln(w, "Target\tStartup Time")
	for label, duration := range services {
		fmt.Fprintf(w, "%s\t%s\n", label, duration)
	}
	test.finishReport(w, label)
}

func (test testRun) finishReport(w *tabwriter.Writer, label string) {
	if test.cmd != nil {
		state := test.cmd.ProcessState
		fmt.Fprintf(w, "%s\t%s\t%s\n", label, state.UserTime(), state.SystemTime())
	}
	fmt.Fprintln(w)
	must(w.Flush())
}

func warnNoServiceReload(testLabel string) {
	fmt.Println()
	fmt.Println("###########################################################################################")
	fmt.Println("  Detected that you are running under ibazel, but do not have per-service-reload enabled.")
	fmt.Println("  In this configuration, services will not be restarted when their code changes.")
	fmt.Println("  If this was unintentional, you can retry with per-service-reload enabled:")
	fmt.Println("")
	fmt.Printf("  `bazel run --@rules_itest//:enable_per_service_reload %s`\n", testLabel)
	fmt.Println("###########################################################################################")
	fmt.Println()
	fmt.Println()
}

func discardServiceErrors(servicesErrCh <-chan error) {
	// Discard errors already queued by services from the previous build.
	// See https://github.com/hermeticbuild/rules_itest/issues/72.
	for {
		select {
		case err := <-servicesErrCh:
			log.Printf("Discarding pending service error before reload: %v", err)
		default:
			return
		}
	}
}

func readServiceSpecs(
	path string,
) (
	map[string]svclib.ServiceSpec, error,
) {
	data, err := os.ReadFile(path)
	must(err)

	var serviceSpecs map[string]svclib.ServiceSpec
	err = json.Unmarshal(data, &serviceSpecs)
	return serviceSpecs, err
}

type portRequest struct {
	label    string
	name     string
	port     string
	reusable bool
}

func servicePortRequests(serviceSpecs map[string]svclib.ServiceSpec) []portRequest {
	var fixed, dynamic []portRequest
	for label, spec := range serviceSpecs {
		add := func(name, port string) {
			request := portRequest{label: label, name: name, port: port, reusable: spec.SoReuseportAware}
			// Match net.Listen's interpretation of all spellings of port zero.
			if n, err := net.LookupPort("tcp", port); err == nil && n == 0 {
				dynamic = append(dynamic, request)
			} else {
				fixed = append(fixed, request)
			}
		}
		if spec.AutoassignPort {
			add("", spec.Port)
		}
		for name, port := range spec.NamedPorts {
			add(name, port)
		}
	}
	return append(fixed, dynamic...)
}

func assignPorts(
	serviceSpecs map[string]svclib.ServiceSpec,
	svcctlAddress string,
) (
	_ svclib.Ports, _ map[string][]io.Closer, _ net.Listener, resultErr error,
) {
	var toClose []io.Closer
	reservedPorts := map[string][]io.Closer{}
	var controlListener net.Listener
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closePorts(toClose))
			closeReservedPorts(reservedPorts)
			if controlListener != nil {
				resultErr = errors.Join(resultErr, controlListener.Close())
			}
		}
	}()
	ports := svclib.Ports{}

	// Keep every fixed port reserved before letting the OS choose any dynamic port.
	for _, request := range servicePortRequests(serviceSpecs) {
		name := qualifiedPortName(request.label, request.name)
		if _, exists := ports[name]; exists {
			return nil, nil, nil, fmt.Errorf("duplicate port name %q", name)
		}
		port := request.port
		var reservedPort io.Closer
		var err error
		if request.reusable {
			requestedPort, parseErr := strconv.Atoi(port)
			if parseErr != nil || requestedPort < 0 || requestedPort > 65535 {
				return nil, nil, nil, fmt.Errorf("invalid port %q for %s", port, request.label)
			}
			reservedPort, port, err = reserveReusablePort(requestedPort)
			if err != nil {
				return nil, nil, nil, err
			}
		} else {
			// We do a bit of a dance here to set SO_LINGER to 0. For details, see
			// https://stackoverflow.com/questions/71975992/what-really-is-the-linger-time-that-can-be-set-with-so-linger-on-sockets
			lc := net.ListenConfig{
				Control: func(network, address string, conn syscall.RawConn) error {
					var setSockoptErr error
					err := conn.Control(func(fd uintptr) {
						setSockoptErr = setSockoptsForPortAssignment(fd, &syscall.Linger{
							Onoff:  1,
							Linger: 0,
						})
					})
					if err != nil {
						return err
					}
					return setSockoptErr
				},
			}

			listener, listenErr := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+port)
			if listenErr != nil {
				return nil, nil, nil, listenErr
			}
			_, port, err = net.SplitHostPort(listener.Addr().String())
			if err != nil {
				listener.Close()
				return nil, nil, nil, err
			}
			reservedPort = listener
		}

		if !terseOutput {
			log.Printf("Assigning port %s to %s\n", port, name)
		}

		ports[name] = port

		if request.reusable {
			reservedPorts[request.label] = append(reservedPorts[request.label], reservedPort)
		} else {
			toClose = append(toClose, reservedPort)
		}
	}

	if err := assignPortAliases(ports, serviceSpecs); err != nil {
		return nil, nil, nil, err
	}

	// The control listener must be opened while all service ports are still held.
	controlListener, err := net.Listen("tcp", svcctlAddress)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen for svcctl: %w", err)
	}
	// Ordinary services need their ports released before they can bind, which
	// leaves a race with other processes. Reusable reservations remain held.
	err = closePorts(toClose)
	toClose = nil
	if err != nil {
		return nil, nil, nil, err
	}

	// Complete hack - we have observed that the ports may not be ready immediately after closing, even with SO_LINGER set to 0.
	// Give the kernel a bit of time to figure out what we've done.
	time.Sleep(10 * time.Millisecond)

	serializedPorts, err := ports.Marshal()
	if err != nil {
		return nil, nil, nil, err
	}
	os.Setenv("ASSIGNED_PORTS", string(serializedPorts))
	return ports, reservedPorts, controlListener, nil
}

func closePorts(ports []io.Closer) error {
	var err error
	for _, port := range ports {
		err = errors.Join(err, port.Close())
	}
	return err
}

func closeReservedPorts(reservedPorts map[string][]io.Closer) {
	for label, ports := range reservedPorts {
		for _, port := range ports {
			if err := port.Close(); err != nil {
				log.Printf("failed to close reusable port reservation for %s: %v\n", label, err)
			}
		}
	}
}

func augmentServiceSpecs(
	serviceSpecs map[string]svclib.ServiceSpec,
	ports svclib.Ports,
	svcctlPort string,
) (
	map[string]svclib.VersionedServiceSpec, error,
) {
	tmpDir := os.Getenv("TMPDIR")
	socketDir := os.Getenv("SOCKET_DIR")

	versionedServiceSpecs := make(map[string]svclib.VersionedServiceSpec, len(serviceSpecs))
	for label, serviceSpec := range serviceSpecs {
		s := svclib.VersionedServiceSpec{
			ServiceSpec: serviceSpec,
		}

		if s.Type == "group" {
			versionedServiceSpecs[label] = s
			continue
		}

		exePath, err := runfiles.Rlocation(s.Exe)
		if err != nil {
			return nil, err
		}
		s.Exe = exePath

		if s.HealthCheck != "" {
			healthCheckPath, err := runfiles.Rlocation(serviceSpec.HealthCheck)
			if err != nil {
				return nil, err
			}
			s.HealthCheck = healthCheckPath
		}

		if serviceSpec.VersionFile != "" {
			versionFilePath, err := runfiles.Rlocation(serviceSpec.VersionFile)
			if err != nil {
				return nil, err
			}

			version, err := os.ReadFile(versionFilePath)
			if err != nil {
				return nil, err
			}
			s.Version = string(version)
		}

		s.Color = logger.Colorize(s.Label)

		if s.AutoassignPort {
			port := ports[s.Label]
			for i := range s.ServiceSpec.Args {
				s.Args[i] = strings.ReplaceAll(s.Args[i], "$${PORT}", port)
			}
			s.HttpHealthCheckAddress = strings.ReplaceAll(s.HttpHealthCheckAddress, "$${PORT}", port)
			for i := range s.ServiceSpec.HealthCheckArgs {
				s.HealthCheckArgs[i] = strings.ReplaceAll(s.HealthCheckArgs[i], "$${PORT}", port)
			}
			for k, v := range s.Env {
				s.Env[k] = strings.ReplaceAll(v, "$${PORT}", port)
			}
		}
		s.Env["SVCCTL_PORT"] = svcctlPort

		versionedServiceSpecs[label] = s
	}

	replacements := make([]Replacement, 0, 2+len(ports))
	replacements = append(replacements,
		Replacement{Old: "$${TMPDIR}", New: tmpDir},
		Replacement{Old: "$${SOCKET_DIR}", New: socketDir},
	)

	for label, port := range ports {
		replacements = append(replacements, Replacement{
			Old: "$${" + label + "}",
			New: port,
		})
	}

	replaceAllPorts := func(s string) string {
		for _, r := range replacements {
			s = strings.ReplaceAll(s, r.Old, r.New)
		}
		return s
	}

	for label, spec := range versionedServiceSpecs {
		spec.HttpHealthCheckAddress = replaceAllPorts(spec.HttpHealthCheckAddress)
		for i := range spec.Args {
			spec.Args[i] = replaceAllPorts(spec.Args[i])
		}
		for i := range spec.HealthCheckArgs {
			spec.HealthCheckArgs[i] = replaceAllPorts(spec.HealthCheckArgs[i])
		}
		for k, v := range spec.Env {
			spec.Env[k] = replaceAllPorts(v)
		}
		versionedServiceSpecs[label] = spec
	}

	return versionedServiceSpecs, nil
}

type Replacement struct {
	Old string
	New string
}

// buildReplacements creates port/env substitution pairs.
// prefix is "$${" for values from JSON files (which preserve literal $$),
// or "${" for values from Bazel args (where $$ is already collapsed to $).
func buildReplacements(ports svclib.Ports, prefix string) []Replacement {
	replacements := make([]Replacement, 0, 2+len(ports))
	replacements = append(replacements,
		Replacement{Old: prefix + "TMPDIR}", New: os.Getenv("TMPDIR")},
		Replacement{Old: prefix + "SOCKET_DIR}", New: os.Getenv("SOCKET_DIR")},
	)
	for label, port := range ports {
		replacements = append(replacements, Replacement{
			Old: prefix + label + "}",
			New: port,
		})
	}
	return replacements
}

func replaceAll(s string, replacements []Replacement) string {
	for _, r := range replacements {
		s = strings.ReplaceAll(s, r.Old, r.New)
	}
	return s
}

func buildTestEnv(ports svclib.Ports) ([]string, error) {
	testEnvPath, err := runfiles.Rlocation(os.Getenv("SVCINIT_TEST_ENV_RLOCATION_PATH"))
	if err != nil {
		panic(err)
	}

	testEnvData, err := os.ReadFile(testEnvPath)
	if err != nil {
		panic(err)
	}

	env := map[string]string{}
	err = json.Unmarshal(testEnvData, &env)
	if err != nil {
		panic(err)
	}

	replacements := buildReplacements(ports, "$${")

	// Note, this can technically specify the same var multiple times.
	// Last one wins - hope that's what you wanted!
	baseEnv := os.Environ()
	for k, v := range env {
		baseEnv = append(baseEnv, k+"="+replaceAll(v, replacements))
	}

	return baseEnv, nil
}
