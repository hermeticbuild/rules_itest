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
	"maps"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
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

const delegatedTargetFlag = "--target_arg"
const delegatedTargetEnvFlag = "--target_env"

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

	// Unix sockets have a 108-character path limit, and the macOS temporary directory can exceed it.
	// Use /tmp on macOS and Go's platform-specific temporary directory on other platforms.
	socketTempDir := ""
	if runtime.GOOS == "darwin" {
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

	unversionedSpecs, aliases, err := readServiceSpecs(serviceSpecsPath)
	must(err)
	if testLabel == "" {
		err = appendDelegatedTargetConfig(unversionedSpecs, aliases, os.Args[1:])
		must(err)
	}

	// Make sure we grab the svcctl port before we assign test ports,
	// otherwise we might steal an assigned port by accident.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)

	ports, reservedPorts, err := assignPorts(unversionedSpecs)
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
			unversionedSpecs, aliases, err := readServiceSpecs(serviceSpecsPath)
			must(err)
			if testLabel == "" {
				err = appendDelegatedTargetConfig(unversionedSpecs, aliases, os.Args[1:])
				must(err)
			}

			serviceSpecs, err := augmentServiceSpecs(unversionedSpecs, ports, svcctlPortStr)
			must(err)

			testCancel()

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
	}
}

func readServiceSpecs(
	path string,
) (
	map[string]svclib.ServiceSpec, map[string][]string, error,
) {
	data, err := os.ReadFile(path)
	must(err)

	var graph struct {
		Services map[string]svclib.ServiceSpec `json:"services"`
		Aliases  map[string][]string           `json:"aliases"`
	}
	err = json.Unmarshal(data, &graph)
	if err != nil {
		return nil, nil, err
	}
	if graph.Services == nil {
		graph.Services = map[string]svclib.ServiceSpec{}
	}
	if graph.Aliases == nil {
		graph.Aliases = map[string][]string{}
	}
	return graph.Services, graph.Aliases, nil
}

func assignPorts(
	serviceSpecs map[string]svclib.ServiceSpec,
) (
	svclib.Ports, map[string][]io.Closer, error,
) {
	var toClose []io.Closer
	reservedPorts := map[string][]io.Closer{}
	ports := svclib.Ports{}

	for label, spec := range serviceSpecs {
		namedPorts := maps.Clone(spec.NamedPorts)
		if spec.AutoassignPort {
			namedPorts[""] = spec.Port
		}

		// Note, this can cause collisions. So be careful!
		// To avoid port collisions, set so_reuseport_aware on the service definition
		// and use SO_REUSEPORT on Unix or SO_REUSEADDR on Windows in your services.
		for portName, port := range namedPorts {
			var reservedPort io.Closer
			var err error
			if spec.SoReuseportAware {
				requestedPort, parseErr := strconv.Atoi(port)
				if parseErr != nil || requestedPort < 0 || requestedPort > 65535 {
					return nil, nil, fmt.Errorf("invalid port %q for %s", port, label)
				}
				reservedPort, port, err = reserveReusablePort(requestedPort)
				if err != nil {
					return nil, nil, err
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
					return nil, nil, listenErr
				}
				_, port, err = net.SplitHostPort(listener.Addr().String())
				if err != nil {
					listener.Close()
					return nil, nil, err
				}
				reservedPort = listener
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
				toClose = append(toClose, reservedPort)
			} else {
				reservedPorts[label] = append(reservedPorts[label], reservedPort)
			}
		}
	}

	for _, reservedPort := range toClose {
		err := reservedPort.Close()
		if err != nil {
			return nil, nil, err
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
		return nil, nil, err
	}
	os.Setenv("ASSIGNED_PORTS", string(serializedPorts))
	return ports, reservedPorts, nil
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

type delegatedTargetConfig struct {
	Args map[string][]string
	Env  map[string]map[string]string
}

func appendDelegatedTargetConfig(serviceSpecs map[string]svclib.ServiceSpec, aliases map[string][]string, rawArgs []string) error {
	config, err := parseDelegatedTargetConfig(rawArgs, serviceSpecs, aliases)
	if err != nil {
		return err
	}

	for label, args := range config.Args {
		spec := serviceSpecs[label]
		spec.Args = append(spec.Args, args...)
		serviceSpecs[label] = spec
	}
	for label, env := range config.Env {
		spec := serviceSpecs[label]
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		for key, value := range env {
			spec.Env[key] = value
		}
		serviceSpecs[label] = spec
	}

	return nil
}

func parseDelegatedTargetConfig(rawArgs []string, serviceSpecs map[string]svclib.ServiceSpec, aliases map[string][]string) (delegatedTargetConfig, error) {
	config := delegatedTargetConfig{
		Args: map[string][]string{},
		Env:  map[string]map[string]string{},
	}
	currentTarget := ""

	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		switch {
		case arg == delegatedTargetFlag:
			if i+1 >= len(rawArgs) {
				return delegatedTargetConfig{}, fmt.Errorf("missing target after %s", delegatedTargetFlag)
			}
			label, err := resolveDelegatedTarget(rawArgs[i+1], serviceSpecs, aliases)
			if err != nil {
				return delegatedTargetConfig{}, err
			}
			currentTarget = label
			if _, ok := config.Args[label]; !ok {
				config.Args[label] = nil
			}
			i++
		case arg == delegatedTargetEnvFlag:
			if i+2 >= len(rawArgs) {
				return delegatedTargetConfig{}, fmt.Errorf("expected %s <target> <key=value>", delegatedTargetEnvFlag)
			}
			label, err := resolveDelegatedTarget(rawArgs[i+1], serviceSpecs, aliases)
			if err != nil {
				return delegatedTargetConfig{}, err
			}
			key, value, err := parseDelegatedEnvAssignment(rawArgs[i+2])
			if err != nil {
				return delegatedTargetConfig{}, err
			}
			if _, ok := config.Env[label]; !ok {
				config.Env[label] = map[string]string{}
			}
			config.Env[label][key] = value
			i += 2
		default:
			if currentTarget == "" {
				return delegatedTargetConfig{}, fmt.Errorf("unexpected argument %q: expected %s <target> before delegated args", arg, delegatedTargetFlag)
			}
			config.Args[currentTarget] = append(config.Args[currentTarget], arg)
		}
	}

	return config, nil
}

func parseDelegatedTargetArgs(rawArgs []string, serviceSpecs map[string]svclib.ServiceSpec, aliases map[string][]string) (map[string][]string, error) {
	config, err := parseDelegatedTargetConfig(rawArgs, serviceSpecs, aliases)
	if err != nil {
		return nil, err
	}

	return config.Args, nil
}

func parseDelegatedEnvAssignment(raw string) (string, string, error) {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || key == "" {
		return "", "", fmt.Errorf("invalid delegated env assignment %q: expected KEY=VALUE", raw)
	}
	return key, value, nil
}

func resolveDelegatedTarget(target string, serviceSpecs map[string]svclib.ServiceSpec, aliases map[string][]string) (string, error) {
	matches := []string{}
	groupMatches := []string{}
	for label, spec := range serviceSpecs {
		if !delegatedTargetMatches(target, label) {
			continue
		}
		if spec.Type == "group" {
			groupMatches = append(groupMatches, label)
			continue
		}
		matches = append(matches, label)
	}
	for alias, labels := range aliases {
		if !delegatedTargetMatches(target, alias) {
			continue
		}
		for _, label := range labels {
			spec, ok := serviceSpecs[label]
			if !ok {
				return "", fmt.Errorf("delegated target alias %q points at unknown itest target %q", alias, label)
			}
			if spec.Type == "group" {
				groupMatches = append(groupMatches, alias+" -> "+label)
				continue
			}
			matches = append(matches, label)
		}
	}

	matches = uniqueSorted(matches)
	groupMatches = uniqueSorted(groupMatches)
	sort.Strings(matches)
	sort.Strings(groupMatches)

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(groupMatches) > 0 {
			return "", fmt.Errorf("delegated target %q refers to a non-executable itest_service_group: %s", target, strings.Join(groupMatches, ", "))
		}
		return "", fmt.Errorf("delegated target %q not found. Available executable itest targets: %s", target, strings.Join(executableItestTargets(serviceSpecs), ", "))
	default:
		return "", fmt.Errorf("delegated target %q is ambiguous. Matches: %s", target, strings.Join(matches, ", "))
	}
}

func delegatedTargetMatches(target string, label string) bool {
	if target == label {
		return true
	}

	withoutModule := strings.TrimPrefix(label, "@@")
	if target == withoutModule {
		return true
	}

	colon := strings.LastIndex(label, ":")
	if colon >= 0 {
		targetName := label[colon+1:]
		if target == targetName || target == ":"+targetName {
			return true
		}
	}

	return false
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := map[string]struct{}{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func executableItestTargets(serviceSpecs map[string]svclib.ServiceSpec) []string {
	targets := make([]string, 0, len(serviceSpecs))
	for label, spec := range serviceSpecs {
		if spec.Type == "group" {
			continue
		}
		targets = append(targets, label)
	}
	return uniqueSorted(targets)
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
