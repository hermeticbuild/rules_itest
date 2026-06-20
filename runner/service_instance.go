package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"rules_itest/logger"
	"rules_itest/svclib"
)

type ServiceInstance struct {
	svclib.VersionedServiceSpec
	stdin io.WriteCloser
	cmd   *exec.Cmd

	startTime     time.Time
	startDuration time.Duration

	startErrFn func() error
	waitErrFn  func() error

	mu                   sync.Mutex
	runErr               error
	killed               bool
	healthcheckAttempted bool
	done                 bool
}

func (s *ServiceInstance) Start(ctx context.Context) error {
	if s.isRunning() {
		return fmt.Errorf("%s is already running", s.Colorize(s.Label))
	}

	// If the process has finished running, we need to reinitialize the cmd.
	if err := initializeServiceCmd(ctx, s); err != nil {
		return err
	}
	s.mu.Lock()
	s.startTime = time.Now()
	s.mu.Unlock()
	return s.startErrFn()
}

func (s *ServiceInstance) WaitUntilHealthy(ctx context.Context) error {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.startDuration = time.Since(s.startTime)
	}()

	if s.Type == "group" {
		return nil
	}

	coloredLabel := s.Colorize(s.Label)
	if s.Type == "task" {
		err := s.waitErrFn()
		log.Printf("%s completed.\n", coloredLabel)
		return err
	}

	sleepDuration, err := time.ParseDuration(s.HealthCheckInterval)
	if err != nil {
		log.Printf("failed to parse health check time duration, falling back to 200ms: %v", err)
		// This should really not happen if we validate it properly in starlark
		sleepDuration = time.Duration(200) * time.Millisecond
	}

	expectedStartDuration, err := time.ParseDuration(s.ExpectedStartDuration)
	if err != nil {
		log.Print("failed to parse expected start duration")
	}

	for {
		if err := s.Error(); err != nil {
			return err
		}

		if s.isDone() {
			state := s.cmd.ProcessState
			if state != nil {
				return fmt.Errorf("%s exited before becoming healthy: %s", coloredLabel, state.String())
			}
			return fmt.Errorf("%s exited before becoming healthy", coloredLabel)
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if s.HealthCheck(ctx, expectedStartDuration) {
			log.Printf("%s healthy!\n", coloredLabel)
			break
		}

		time.Sleep(sleepDuration)
	}

	return nil
}

var httpClient = http.Client{
	// It's important to have a reasonable timeout here since the connection may never get accepted
	// if it's to a port that is SO_REUSEPORT-aware. In that case, the healthcheck will hang forever
	// without this timeout.
	Timeout: 50 * time.Millisecond,
}

func (s *ServiceInstance) HealthCheck(ctx context.Context, expectedStartDuration time.Duration) bool {
	coloredLabel := s.Colorize(s.Label)
	shouldSilence := s.startTime.Add(expectedStartDuration).After(time.Now())

	isHealthy := true
	var err error
	if s.HttpHealthCheckAddress != "" {
		httpHealthCheckReq, err := http.NewRequestWithContext(ctx, "GET", s.HttpHealthCheckAddress, nil)
		if err != nil {
			log.Printf("Failed to construct healthcheck request for %s: %v\n", coloredLabel, err)
			return false
		}

		if !s.HealthcheckAttempted() || !shouldSilence {
			log.Printf("HTTP Healthchecking %s (pid %d) : %s\n", coloredLabel, s.Pid(), s.HttpHealthCheckAddress)
		}

		logFunc := log.Printf
		if shouldSilence {
			logFunc = func(format string, v ...any) {}
		}

		var resp *http.Response
		resp, err = httpClient.Do(httpHealthCheckReq)
		if err != nil {
			logFunc("healthcheck for %s failed: %v\n", coloredLabel, err)
			isHealthy = false
		} else if resp != nil {
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
				logFunc("healthcheck for %s failed: %v\n", coloredLabel, resp)
				isHealthy = false
			}

			closeErr := resp.Body.Close()
			if closeErr != nil {
				logFunc("error closing http body %v", closeErr)
			}
		}

	} else if s.ServiceSpec.HealthCheck != "" {
		if !s.HealthcheckAttempted() || !shouldSilence {
			if terseOutput {
				log.Printf("CMD Healthchecking %s\n", coloredLabel)
			} else {
				log.Printf("CMD Healthchecking %s (pid %d) : %s %v\n", coloredLabel, s.Pid(), s.Colorize(s.HealthCheckLabel), strings.Join(s.HealthCheckArgs, " "))
			}
		}

		cmd := exec.CommandContext(ctx, s.ServiceSpec.HealthCheck, s.HealthCheckArgs...)
		if shouldSilence {
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
		} else {
			cmd.Stdout = logger.New(s.Label+"? ", s.Color, os.Stdout)
			cmd.Stderr = logger.New(s.Label+"? ", s.Color, os.Stderr)
		}
		err = cmd.Run()
		if err != nil {
			cmd.Stdout.Write([]byte(err.Error()))
			isHealthy = false
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthcheckAttempted = true
	return isHealthy
}

func (s *ServiceInstance) HealthcheckAttempted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthcheckAttempted
}

func (s *ServiceInstance) StartTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startTime
}

func (s *ServiceInstance) StartDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startDuration
}

func (s *ServiceInstance) Error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

func (s *ServiceInstance) Stop() error {
	var signal syscall.Signal
	switch s.ShutdownSignal {
	case "SIGKILL":
		signal = syscall.SIGKILL
	case "SIGTERM":
		signal = syscall.SIGTERM
	default:
		// Default to SIGKILL if unspecified or unrecognized. In case we add new values to itest.bzl but forget to add it here
		signal = syscall.SIGKILL
	}

	return s.StopWithSignal(signal)
}

func isGone(err error) bool {
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}

	var errno syscall.Errno
	return errors.As(err, &errno) && errnoMeansProcessGone(errno)
}

func (s *ServiceInstance) StopWithSignal(signal syscall.Signal) error {
	if s.cmd.Process == nil {
		return nil
	}

	err := killGroup(s.cmd, signal)
	if isGone(err) {
		return nil
	}

	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.killed = true
	}()

	// If process already exited, still attempt to clean up any remaining
	// process group members (children may outlive the group leader).
	if s.isDone() {
		_ = killGroup(s.cmd, signal)
		return nil
	}

	if signal == syscall.SIGKILL {
		log.Printf("Sent SIGKILL to %s\n", s.Colorize(s.Label))
		for !s.isDone() {
			time.Sleep(5 * time.Millisecond)
		}
	} else {
		log.Printf("Sent SIGTERM to %s\n", s.Colorize(s.Label))

		shutdownTimeout, err := time.ParseDuration(s.VersionedServiceSpec.ShutdownTimeout)
		if err != nil {
			log.Printf("failed to parse health check timeout, falling back to no timeout: %v", err)
			return err
		}

		log.Printf("Sent SIGTERM to %s, waiting for %s to stop gracefully\n", s.Colorize(s.Label), s.VersionedServiceSpec.ShutdownTimeout)

		waitFor := time.After(shutdownTimeout)
		for {
			// Check if the process has exited
			if s.isDone() {
				break
			}

			select {
			case <-waitFor:
				log.Printf("WARNING: %s did not exit within %s, sending SIGKILL. If you are trying to collect coverage, you will most likely miss stats, try increasing the default shutdown timeout flag (--@rules_itest//:shutdown_timeout) or the service `shutdown_timeout` attribute.\n", s.Colorize(s.Label), shutdownTimeout)

				err := killGroup(s.cmd, syscall.SIGKILL)
				if err != nil {
					if isGone(err) {
						err = nil
					}
					return err
				}

				for !s.isDone() {
					time.Sleep(5 * time.Millisecond)
				}

				if s.EnforceForcefulShutdown {
					return fmt.Errorf("%s did not handle SIGTERM within it's shutdown timeout. Consider raising it's `shutdown_timeout` attribute or set `shutdown_signal` to SIGKILL if graceful shutdown is not needed", s.Label)
				}

				return nil
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}
	}

	return nil
}

func (s *ServiceInstance) Wait() error {
	err := s.waitErrFn()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.runErr = err

	return err
}

func (s *ServiceInstance) Pid() int {
	return s.cmd.Process.Pid
}

func (s *ServiceInstance) ProcessState() *os.ProcessState {
	return s.cmd.ProcessState
}

// Returns true if killed by svcinit / svcctl instead of exited normally or crashed.
func (s *ServiceInstance) Killed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killed
}

func (s *ServiceInstance) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done && s.cmd.ProcessState != nil
}

func (s *ServiceInstance) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil &&
		s.cmd.Process != nil &&
		!s.done &&
		!s.killed
}

func (s *ServiceInstance) SetDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
}
