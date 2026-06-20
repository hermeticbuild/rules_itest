package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
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

	interactiveCh := make(chan string, 100)
	if shouldHotReload {
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

	// Sockets have a short max path length (108 chars) so the TEST_TMPDIR path is way too long.
	// Put them in the OS temp location - note that this is per-test (i.e. hermetic) on linux anyway.
	socketDir, err := os.MkdirTemp("", "")
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

	// Make sure we grab the svcctl port before we assign test ports,
	// otherwise we might steal an assigned port by accident.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)

	ports, err := assignPorts(unversionedSpecs)
	must(err)

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
		testCtx, testCancel := context.WithCancel(ctx)
		testErrCh := make(chan error, 1)
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

			testCmd = exec.CommandContext(testCtx, testPath, testArgs...)
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

			go func() {
				testErrCh <- testCmd.Wait()

				testDuration := time.Since(testStartTime)
				log.Printf("Test duration: %s\n", testDuration)
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

		select {
		case <-ctx.Done():
			log.Println("Shutting down services.")
			mustStopAllForExit()
			log.Println("Cleaning up.")
			return
		case ibazelCmd := <-interactiveCh:
			log.Println(ibazelCmd)

			// Restart any services as needed.
			unversionedSpecs, err := readServiceSpecs(serviceSpecsPath)
			must(err)

			serviceSpecs, err := augmentServiceSpecs(unversionedSpecs, ports, svcctlPortStr)
			must(err)

			testCancel()

			// TODO(zbarsky): what is the right behavior here when services are crashing in ibazel mode?

			// This is a brittle way of draining a channel in a nonblocking way,
			// consider instead signalling cancellation of the services with a
			// context, letting them close the channel, and using a waitgroup to
			// wait for them to exit.
		Drain:
			for {
				select {
				case <-servicesErrCh:
					// nothing
				default:
					break Drain
				}
			}

			criticalPath, err = r.UpdateSpecsAndRestart(serviceSpecs, servicesErrCh, []byte(ibazelCmd))
			must(err)

			continue

		case testErr := <-testErrCh:
			if testErr != nil {
				log.Printf("Encountered error during test run: %s\n", testErr)
				if isOneShot {
					mustStopAllForExit()
					os.Exit(1)
				}
			}
		case serviceErr := <-servicesErrCh:
			log.Print(serviceErr)
			if isOneShot {
				mustStopAllForExit()
				log.Fatal("Service exited uncleanly, marking test as failed.\n\n")
			}
		}

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

		if isOneShot {
			break
		}

		if shouldHotReload && !enablePerServiceReload {
			fmt.Println()
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

		select {
		case <-ctx.Done():
			log.Println("Shutting down services.")
			_, err := r.StopAll()
			must(err)
			log.Println("Cleaning up.")
			return
		case ibazelCmd := <-interactiveCh:
			log.Println(ibazelCmd)

			// Restart any services as needed.
			unversionedSpecs, err := readServiceSpecs(serviceSpecsPath)
			must(err)

			serviceSpecs, err := augmentServiceSpecs(unversionedSpecs, ports, svcctlPortStr)
			must(err)

		// Non-blocking drain of any pending service crash errors before restarting.
		for {
			select {
			case crashErr := <-servicesErrCh:
				log.Printf("Discarding pending service error before reload: %v", crashErr)
			default:
				goto drained
			}
		}
	drained:
			criticalPath, err = r.UpdateSpecsAndRestart(serviceSpecs, servicesErrCh, []byte(ibazelCmd))
			must(err)
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

func assignPorts(
	serviceSpecs map[string]svclib.ServiceSpec,
) (
	svclib.Ports, error,
) {
	var toClose []net.Listener
	ports := svclib.Ports{}

	for label, spec := range serviceSpecs {
		namedPorts := maps.Clone(spec.NamedPorts)
		if spec.AutoassignPort {
			namedPorts[""] = spec.Port
		}

		// Note, this can cause collisions. So be careful!
		// To avoid port collisions, set the `so_reuseport_aware` option on the service definition
		// and use the SO_REUSEPORT socket option in your services.
		for portName, port := range namedPorts {
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

			listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+port)
			if err != nil {
				return nil, err
			}
			_, port, err = net.SplitHostPort(listener.Addr().String())
			if err != nil {
				return nil, err
			}

			qualifiedPortName := label
			if portName != "" {
				qualifiedPortName += "." + portName
			}

			if !terseOutput {
				log.Printf("Assigning port %s to %s\n", port, qualifiedPortName)
			}

			ports.Set(qualifiedPortName, port)

			{
				// TODO(zbarsky): Clean this up after April 2026
				qualifiedPortName := label
				if portName != "" {
					qualifiedPortName += ":" + portName
				}

				if !terseOutput {
					log.Printf("Assigning port %s to %s\n", port, qualifiedPortName)
				}

				ports.Set(qualifiedPortName, port)
			}

			if !spec.SoReuseportAware {
				toClose = append(toClose, listener)
			}
		}
	}

	for _, listener := range toClose {
		err := listener.Close()
		if err != nil {
			return nil, err
		}
	}

	for label, spec := range serviceSpecs {
		for portName, aliasedTo := range spec.PortAliases {
			qualifiedPortName := label
			if portName != "" {
				qualifiedPortName += "." + portName
			}

			ports.Set(qualifiedPortName, ports[aliasedTo])

			{
				// TODO(zbarsky): Clean this up after April 2026
				qualifiedPortName := label
				if portName != "" {
					qualifiedPortName += ":" + portName
				}

				ports.Set(qualifiedPortName, ports[aliasedTo])
			}
		}
	}

	// Complete hack - we have observed that the ports may not be ready immediately after closing, even with SO_LINGER set to 0.
	// Give the kernel a bit of time to figure out what we've done.
	time.Sleep(10 * time.Millisecond)

	serializedPorts, err := ports.Marshal()
	if err != nil {
		return nil, err
	}
	os.Setenv("ASSIGNED_PORTS", string(serializedPorts))
	return ports, nil
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
