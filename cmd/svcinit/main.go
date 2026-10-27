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
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"rules_itest/logger"
	"rules_itest/runner"
	"rules_itest/svcctl"
	"rules_itest/svclib"
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
	runStart := time.Now()

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

	var interactiveCh chan string
	if shouldHotReload {
		interactiveCh = make(chan string, 100)
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				// TODO: better notification setup needed
				interactiveCh <- scanner.Text()
				//close(interactiveCh)
				//interactiveCh = make(chan struct{})
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

	activeSpecs, err := readServiceSpecs(serviceSpecsPath)
	must(err)

	// Make sure we grab the svcctl port before we assign test ports,
	// otherwise we might steal an assigned port by accident.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)

	ports, reservedPorts, err := assignPorts(activeSpecs)
	must(err)
	defer func() {
		if err := closePorts(reservedPorts); err != nil {
			log.Printf("failed to close reusable port reservations: %v", err)
		}
	}()

	svcctlPort := listener.Addr().(*net.TCPAddr).Port
	svcctlPortStr := strconv.Itoa(svcctlPort)
	os.Setenv("SVCCTL_PORT", svcctlPortStr)

	if testLabel == "" {
		err = os.WriteFile("/tmp/svcctl_port", []byte(svcctlPortStr), 0600)
		must(err)
		defer os.Remove("/tmp/svcctl_port")
	}

	serviceSpecs, err := augmentServiceSpecs(activeSpecs, ports, svcctlPortStr)
	must(err)

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	r, err := runner.New(ctx, serviceSpecs)
	must(err)

	mustStopAllForExit := sync.OnceValue(func() map[string]*os.ProcessState {
		states, err := r.StopAll()
		cancelFunc()
		must(err)
		return states
	})

	servicesErrCh := make(chan error, len(activeSpecs))

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

	// API is                 NewWriter(output io.Writer, minwidth, tabwidth, padding int, padchar byte, flags uint) *Writer
	reportWriter := tabwriter.NewWriter(os.Stdout, 0, 8, 8, ' ', 0)
	buf := bytes.NewBuffer(nil)

	for {
		buf.WriteString("\nTarget\tCritical Path Contribution\n")
		for _, task := range criticalPath {
			buf.WriteString(fmt.Sprintf("%s\t%s\n", task.Key(), task.Duration()))
		}
		_, err := reportWriter.Write(buf.Bytes())
		must(err)
		buf.Reset()
		err = reportWriter.Flush()
		must(err)

		var testCmd *exec.Cmd
		var testCancel context.CancelFunc
		var testErrCh chan error
		if testLabel != "" {
			// Bazel's args attribute converts $$ to $, so args arrive with
			// single-$ placeholders (e.g. ${@@//:svc}) unlike env/spec files
			// which preserve the literal $$ since they're read from JSON.
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
			testCancel = cancel
			testCmd = exec.CommandContext(testCtx, testPath, testArgs...)
			testCmd.Env = testEnv

			// Adjust remaining timeout to account for service startup.
			timeout := os.Getenv("TEST_TIMEOUT")
			if timeout != "" {
				timeoutVal, err := strconv.Atoi(timeout)
				if err != nil {
					fmt.Println(err)
				} else {
					timeoutVal -= int(math.Ceil(testStartTime.Sub(runStart).Seconds()))
					testCmd.Env = append(testCmd.Env, "TEST_TIMEOUT="+strconv.Itoa(timeoutVal))
				}
			}

			testCmd.Stdout = os.Stdout
			testCmd.Stderr = os.Stderr

			if err := testCmd.Start(); err != nil {
				panic(err)
			}

			testErrCh = make(chan error, 1)
			go func() {
				err := testCmd.Wait()
				testDuration := time.Since(testStartTime)
				log.Printf("Test duration: %s\n", testDuration)
				testErrCh <- err
			}()
		}

		fmt.Println()

		if shouldHotReload && !enablePerServiceReload {
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

		prepareReload := func(notification []byte) (func(), error) {
			next, err := readServiceSpecs(serviceSpecsPath)
			if err != nil {
				return nil, fmt.Errorf("read service specs: %w", err)
			}
			if err := checkPortReload(activeSpecs, next, ports); err != nil {
				return nil, err
			}

			serviceSpecs, err := augmentServiceSpecs(next, ports, svcctlPortStr)
			if err != nil {
				return nil, fmt.Errorf("augment service specs: %w", err)
			}

			return func() {
				// This is a brittle way of draining a channel in a nonblocking way,
				// consider instead signalling cancellation of the services with a
				// context, letting them close the channel, and using a waitgroup to
				// wait for them to exit.
				// See: https://github.com/hermeticbuild/rules_itest/issues/72
			Drain:
				for {
					select {
					case crashErr := <-servicesErrCh:
						log.Printf("Discarding pending service error before reload: %v", crashErr)
					default:
						break Drain
					}
				}

				runStart = startRun(func() {
					criticalPath, err = r.UpdateSpecsAndRestart(serviceSpecs, servicesErrCh, notification)
					must(err)
					activeSpecs = next
				})
			}, nil
		}
		report := func() {
			testCancel()
			if isOneShot {
				buf.WriteString("Target\tUser Time\tSystem Time\n")
				states := mustStopAllForExit()
				for label, state := range states {
					buf.WriteString(fmt.Sprintf("%s\t%s\t%s\n",
						label, state.UserTime(), state.SystemTime()))
				}
			} else {
				buf.WriteString("Target\tStartup Time\n")
				durations := r.GetStartDurations()
				for label, duration := range durations {
					buf.WriteString(fmt.Sprintf("%s\t%s\n", label, duration))
				}
			}

			if testLabel != "" {
				buf.WriteString(fmt.Sprintf("%s\t%s\t%s\n",
					testLabel, testCmd.ProcessState.UserTime(), testCmd.ProcessState.SystemTime()))
			}
			buf.WriteRune('\n')
			_, err = reportWriter.Write(buf.Bytes())
			must(err)
			buf.Reset()
			err = reportWriter.Flush()
			must(err)
		}

		switch waitForRun(ctx, isOneShot, interactiveCh, testErrCh, servicesErrCh, testCancel, prepareReload, report) {
		case runComplete:
			if isOneShot {
				return
			}
		case runTestFailed:
			mustStopAllForExit()
			os.Exit(1)
		case runServiceFailed:
			mustStopAllForExit()
			log.Fatal("Service exited uncleanly, marking test as failed.\n\n")
		case runShutdown:
			log.Println("Shutting down services.")
			mustStopAllForExit()
			log.Println("Cleaning up.")
			return
		}
	}
}

func startRun(startServices func()) time.Time {
	start := time.Now()
	startServices()
	return start
}

const successfulBuild = "IBAZEL_BUILD_COMPLETED SUCCESS"

type runEvent int

const (
	runComplete runEvent = iota
	runTestFailed
	runServiceFailed
	runShutdown
)

// A nil builds channel means the run is not managed by iBazel; a nil tests
// channel means there is no active test. prepareReload validates a build and
// returns the service restart to perform after the current test has finished.
func waitForRun(ctx context.Context, oneShot bool, builds <-chan string, tests, services <-chan error, cancelTest context.CancelFunc, prepareReload func([]byte) (func(), error), report func()) runEvent {
	for {
		var testErr error
		var restart func()
		cancelRequested := false
		select {
		case <-ctx.Done():
			return runShutdown
		case cmd := <-builds:
			log.Println(cmd)
			if cmd != successfulBuild {
				continue
			}
			var err error
			restart, err = prepareReload([]byte(cmd + "\n"))
			if err != nil {
				log.Printf("Skipping reload: %v", err)
				continue
			}
			if tests != nil {
				select {
				case testErr = <-tests:
				default:
					cancelRequested = true
					cancelTest()
					testErr = <-tests
				}
			}
		case testErr = <-tests:
		case err := <-services:
			log.Print(err)
			if oneShot {
				return runServiceFailed
			}
			continue
		}

		if tests != nil {
			if testErr != nil {
				if cancelRequested {
					log.Printf("Test exited while stopping for reload: %v", testErr)
				} else {
					log.Printf("Encountered error during test run: %s\n", testErr)
				}
			}
			if oneShot {
				// Do not report success if a service failure is already pending.
				select {
				case serviceErr := <-services:
					log.Print(serviceErr)
					return runServiceFailed
				default:
				}
				if testErr != nil {
					return runTestFailed
				}
			}
			report()
			tests = nil
		}
		if restart != nil {
			if ctx.Err() != nil {
				return runShutdown
			}
			restart()
		}
		if oneShot || restart != nil {
			return runComplete
		}
	}
}

func readServiceSpecs(
	path string,
) (
	map[string]svclib.ServiceSpec, error,
) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var serviceSpecs map[string]svclib.ServiceSpec
	err = json.Unmarshal(data, &serviceSpecs)
	return serviceSpecs, err
}

func assignPorts(
	serviceSpecs map[string]svclib.ServiceSpec,
) (
	_ svclib.Ports, _ []io.Closer, err error,
) {
	var toClose, reservedPorts []io.Closer
	defer func() {
		if err != nil {
			err = errors.Join(err, closePorts(toClose), closePorts(reservedPorts))
		}
	}()
	assigned := make(map[portName]string)

	for label, spec := range serviceSpecs {
		// Note, this can cause collisions. So be careful!
		// To avoid port collisions, set so_reuseport_aware on the service definition
		// and use SO_REUSEPORT on Unix or SO_REUSEADDR on Windows in your services.
		for name, port := range namedPorts(spec) {
			key := portName{label, name}
			sockets, port, err := reserveServicePort(spec.PortFamily, port, spec.SoReuseportAware)
			if err != nil {
				return nil, nil, fmt.Errorf("assign port for %s: %w", key, err)
			}
			assigned[key] = port
			if !terseOutput {
				log.Printf("Assigning port %s to %s\n", port, key)
			}

			if !spec.SoReuseportAware {
				toClose = append(toClose, sockets...)
			} else {
				reservedPorts = append(reservedPorts, sockets...)
			}
		}
	}

	err = closePorts(toClose)
	toClose = nil
	if err != nil {
		return nil, nil, fmt.Errorf("close port reservations: %w", err)
	}

	ports, err := publicPorts(assigned, serviceSpecs)
	if err != nil {
		return nil, nil, err
	}

	// Service binds have sometimes failed immediately after reservations were
	// released. Give the kernel time to make the ports available.
	time.Sleep(10 * time.Millisecond)

	serializedPorts, err := ports.Marshal()
	if err != nil {
		return nil, nil, err
	}
	os.Setenv("ASSIGNED_PORTS", string(serializedPorts))
	return ports, reservedPorts, nil
}

func closePorts(ports []io.Closer) error {
	var errs []error
	for _, port := range ports {
		if err := port.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
