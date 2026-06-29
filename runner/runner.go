package runner

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"

	"rules_itest/logger"
	"rules_itest/runner/topological"
	"rules_itest/svclib"
)

// We need to use process groups to reliably tear down services and their descendants.
// This is especially important in hot-reload mode, where you need to restart the child
// and have it bind the same port.
// However, we don't want to do this in tests, because Bazel will already terminate the
// test process (svcinit) and all its children.
// If we were to start new process groups in tests, we could leak children (at least on Mac).
var shouldUseProcessGroups = runtime.GOOS != "windows" && os.Getenv("BAZEL_TEST") != "1"
var terseOutput = os.Getenv("SVCINIT_TERSE_OUTPUT") == "True"

type ServiceSpecs = map[string]svclib.VersionedServiceSpec

type Runner struct {
	ctx          context.Context
	serviceSpecs ServiceSpecs

	serviceInstances map[string]*ServiceInstance
}

func New(ctx context.Context, serviceSpecs ServiceSpecs) (*Runner, error) {
	r := &Runner{
		ctx:              ctx,
		serviceInstances: map[string]*ServiceInstance{},
	}
	err := r.UpdateSpecs(serviceSpecs, nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func colorize(s svclib.VersionedServiceSpec) string {
	return s.Colorize(s.Label)
}

func (r *Runner) StartAll(serviceErrCh chan error) ([]topological.Task, error) {
	tasks := allTasks(r.serviceInstances, func(ctx context.Context, service *ServiceInstance) error {
		if service.Type == "group" {
			return nil
		}

		if service.Deferred {
			log.Printf("Deferring %s\n", colorize(service.VersionedServiceSpec))
			return nil
		}

		if terseOutput {
			log.Printf("Starting %s\n", colorize(service.VersionedServiceSpec))
		} else {
			log.Printf("Starting %s %v\n", colorize(service.VersionedServiceSpec), service.cmd.Args[1:])
		}

		startErr := service.Start(ctx)
		if startErr != nil {
			return startErr
		}

		go func() {
			err := service.Wait()
			if err != nil && !service.Killed() {
				serviceErrCh <- fmt.Errorf(colorize(service.VersionedServiceSpec) + " exited with error: " + err.Error())
			}
		}()

		if service.VersionedServiceSpec.HealthCheckTimeout != "" {
			timeout, err := time.ParseDuration(service.VersionedServiceSpec.HealthCheckTimeout)
			if err != nil {
				log.Printf("failed to parse health check timeout, falling back to no timeout: %v", err)
			}
			timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
			ctx = timeoutCtx
			defer cancel()
		}
		return service.WaitUntilHealthy(ctx)
	})
	starter := topological.NewRunner(tasks)
	err := starter.Run(r.ctx)

	return starter.CriticalPath(), err
}

func (r *Runner) StopAll() (map[string]*os.ProcessState, error) {
	tasks := allTasks(r.serviceInstances, func(ctx context.Context, service *ServiceInstance) error {
		if service.Type == "group" || service.Deferred {
			return nil
		}
		log.Printf("Stopping %s\n", colorize(service.VersionedServiceSpec))
		return service.Stop()
	})
	stopper := topological.NewReversedRunner(tasks)
	err := stopper.Run(r.ctx)

	states := make(map[string]*os.ProcessState)

	for _, serviceInstance := range r.serviceInstances {
		if serviceInstance.Type == "group" || serviceInstance.Deferred {
			continue
		}
		states[serviceInstance.Label] = serviceInstance.ProcessState()
	}

	return states, err
}

func (r *Runner) GetStartDurations() map[string]time.Duration {
	durations := make(map[string]time.Duration)

	for _, serviceInstance := range r.serviceInstances {
		durations[serviceInstance.Label] = serviceInstance.startDuration
	}

	return durations
}

func (r *Runner) GetInstance(label string) *ServiceInstance {
	return r.serviceInstances[label]
}

type updateActions struct {
	toStopLabels   []string
	toStartLabels  []string
	toReloadLabels []string
}

func computeUpdateActions(currentServices, newServices ServiceSpecs) updateActions {
	actions := updateActions{}

	// Check if existing services need a reload, a restart, or a shutdown.
	for label, service := range currentServices {
		newService, ok := newServices[label]
		if !ok {
			fmt.Println(label + " has been removed, stopping")
			actions.toStopLabels = append(actions.toStopLabels, label)
			continue
		}

		// We technically don't need a restart if the change is the list of deps.
		// But that should not be a common use case, so it's not worth the complexity.
		if !reflect.DeepEqual(service, newService) {
			log.Printf(colorize(service) + " definition or code has changed, restarting...")
			if service.HotReloadable && reflect.DeepEqual(service.ServiceSpec, newService.ServiceSpec) {
				// The only difference is the Version. Trust the service that
				// it prefers to receive the ibazel reload command.
				actions.toReloadLabels = append(actions.toReloadLabels, label)
			} else {
				actions.toStopLabels = append(actions.toStopLabels, label)
				actions.toStartLabels = append(actions.toStartLabels, label)
			}
			continue
		}
	}

	// Handle new services
	for label := range newServices {
		if _, ok := currentServices[label]; !ok {
			actions.toStartLabels = append(actions.toStartLabels, label)
		}
	}

	return actions
}

func (r *Runner) UpdateSpecs(serviceSpecs ServiceSpecs, ibazelCmd []byte) error {
	updateActions := computeUpdateActions(r.serviceSpecs, serviceSpecs)

	for _, label := range updateActions.toStopLabels {
		serviceInstance := r.serviceInstances[label]
		if serviceInstance.Type == "group" {
			continue
		}
		serviceInstance.Stop()
		delete(r.serviceInstances, label)
	}

	for _, label := range updateActions.toStartLabels {
		var err error
		r.serviceInstances[label], err = prepareServiceInstance(r.ctx, serviceSpecs[label])
		if err != nil {
			return err
		}
	}

	for _, label := range updateActions.toReloadLabels {
		old := r.serviceInstances[label]
		_, err := old.stdin.Write(ibazelCmd)
		if err != nil {
			// Service likely crashed — fall back to a full restart.
			log.Printf("%s hot-reload stdin write failed, falling back to restart: %v", colorize(old.VersionedServiceSpec), err)
			old.stdin.Close()
			if stopErr := old.StopWithSignal(syscall.SIGKILL); stopErr != nil {
				log.Printf("%s stop during crash fallback failed: %v", colorize(old.VersionedServiceSpec), stopErr)
			}
			delete(r.serviceInstances, label)
			r.serviceInstances[label], err = prepareServiceInstance(r.ctx, serviceSpecs[label])
			if err != nil {
				return err
			}
		}
	}

	r.serviceSpecs = serviceSpecs
	return nil
}

func (r *Runner) UpdateSpecsAndRestart(
	serviceSpecs ServiceSpecs,
	serviceErrCh chan error,
	ibazelCmd []byte,
) (
	[]topological.Task, error,
) {
	err := r.UpdateSpecs(serviceSpecs, ibazelCmd)
	if err != nil {
		return nil, err
	}
	return r.StartAll(serviceErrCh)
}

func prepareServiceInstance(ctx context.Context, s svclib.VersionedServiceSpec) (*ServiceInstance, error) {
	if s.Type == "group" {
		return &ServiceInstance{
			VersionedServiceSpec: s,
			startErrFn:           sync.OnceValue(func() error { return nil }),
		}, nil
	}

	instance := &ServiceInstance{
		VersionedServiceSpec: s,
	}

	err := initializeServiceCmd(ctx, instance)
	if err != nil {
		return nil, err
	}

	return instance, nil
}

func initializeServiceCmd(ctx context.Context, instance *ServiceInstance) error {
	s := instance.VersionedServiceSpec

	cmd := exec.CommandContext(ctx, s.Exe, s.Args...)
	// Note, this leaks the caller's env into the service, so it's not hermetic.
	// For `bazel test`, Bazel is already sanitizing the env, so it's fine.
	// For `bazel run`, there is no expectation of hermeticity, and it can be nice to use env to control behavior.
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = logger.New(s.Label+"> ", s.Color, os.Stdout)
	cmd.Stderr = logger.New(s.Label+"> ", s.Color, os.Stderr)

	if shouldUseProcessGroups {
		setPgid(cmd)
	}

	// Even if a child process exits, Wait will block until the I/O pipes are closed.
	// They may have been forwarded to an orphaned child, so we disable that behavior to unblock exit.
	if s.Type == "service" && !s.Deferred {
		// We need a bit of grace period to allow I/O pipes to close on our end.
		cmd.WaitDelay = 50 * time.Millisecond
	}

	instance.cmd = cmd
	instance.killed = false
	instance.startErrFn = sync.OnceValue(cmd.Start)
	instance.waitErrFn = sync.OnceValue(func() error {
		res := cmd.Wait()
		instance.SetDone()
		return res
	})

	if s.HotReloadable {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		instance.stdin = stdin
	}

	instance.mu.Lock()
	instance.runErr = nil
	instance.done = false
	instance.healthcheckAttempted = false
	instance.mu.Unlock()

	return nil
}
