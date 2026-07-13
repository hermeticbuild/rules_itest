//go:build go1.22

package svcctl

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"syscall"
	"time"

	"rules_itest/runner"
	"rules_itest/svclib"
)

type handlerFn = func(context.Context, *runner.Runner, chan error, http.ResponseWriter, *http.Request)

func handle(ctx context.Context, mux *http.ServeMux, r *runner.Runner, servicesErrCh chan error, pattern string, handler handlerFn) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		handler(ctx, r, servicesErrCh, w, req)
	})
}

func getService(r *runner.Runner, req *http.Request) (*runner.ServiceInstance, int, error) {
	params := req.URL.Query()
	service := params.Get("service")
	if service == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("service parameter is required")
	}

	s := r.GetInstance(service)
	if s == nil {
		return nil, http.StatusBadRequest, fmt.Errorf("instance %q not found", service)
	}

	if s.Type != "service" {
		return nil, http.StatusBadRequest, fmt.Errorf("instance %q is not a service", service)
	}

	return s, http.StatusOK, nil
}

func handleHealthCheck(ctx context.Context, r *runner.Runner, _ chan error, w http.ResponseWriter, req *http.Request) {
	s, status, err := getService(r, req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	isHealthy := s.HealthCheck(ctx, 0)
	if !isHealthy {
		http.Error(w, "Healthcheck failed", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func colorize(s svclib.VersionedServiceSpec) string {
	return s.Colorize(s.Label)
}

func handleStart(ctx context.Context, r *runner.Runner, serviceErrCh chan error, w http.ResponseWriter, req *http.Request) {
	s, status, err := getService(r, req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	if s.Deferred {
		// make sure all the non-deferred dependencies are started
		for _, dep := range s.Deps {
			depService := r.GetInstance(dep)
			if depService == nil {
				http.Error(w, fmt.Sprintf("dependency %q not found", dep), http.StatusInternalServerError)
				return
			}

			if depService.Deferred {
				continue
			}

			depsErr := s.WaitUntilHealthy(ctx)
			if depsErr != nil {
				http.Error(w, fmt.Sprintf("Failed to wait for %q until healthy", dep), http.StatusInternalServerError)
			}
		}
	}

	log.Printf("Starting %s\n", colorize(s.VersionedServiceSpec))

	err = s.Start(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// NOTE: it is important to wait here because we started the service without using `StartAll`,
	// which waits for processes to prevent them from turning into zombies.
	go func() {
		waitErr := s.Wait()
		if waitErr != nil && !s.Killed() {
			serviceErrCh <- fmt.Errorf(s.Colorize(s.Label) + " exited with error: " + waitErr.Error())
		}
	}()

	w.WriteHeader(http.StatusOK)
}

func handleKill(ctx context.Context, r *runner.Runner, _ chan error, w http.ResponseWriter, req *http.Request) {
	params := req.URL.Query()
	signal := params.Get("signal")

	s, status, err := getService(r, req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	// Currently only SIGTERM and SIGKILL are supported.
	switch signal {
	case "":
		err = s.Stop()
	case "SIGTERM":
		err = s.StopWithSignal(syscall.SIGTERM)
	case "SIGKILL":
		err = s.StopWithSignal(syscall.SIGKILL)
	default:
		http.Error(w, "unsupported signal", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func handleWait(ctx context.Context, r *runner.Runner, _ chan error, w http.ResponseWriter, req *http.Request) {
	s, status, err := getService(r, req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	params := req.URL.Query()
	timeout := params.Get("timeout")
	var t time.Duration
	if timeout != "" {
		t, err = time.ParseDuration(timeout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, t)
		ctx = timeoutCtx
		defer cancel()
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- s.Wait()
	}()

	select {
	case <-ctx.Done():
		http.Error(w, "timeout", http.StatusRequestTimeout)
	case err := <-errChan:
		if err == nil {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("0"))
			return
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(fmt.Sprintf("%d", exitErr.ExitCode())))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type portHandler struct {
	ports svclib.Ports
}

func (p portHandler) handle(ctx context.Context, r *runner.Runner, _ chan error, w http.ResponseWriter, req *http.Request) {
	params := req.URL.Query()
	service := params.Get("service")
	if service == "" {
		http.Error(w, "service parameter is required", http.StatusBadRequest)
		return
	}

	port, ok := p.ports[service]
	if !ok {
		http.Error(w, "port is not autoassigned", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(port))
}

func Serve(ctx context.Context, listener net.Listener, r *runner.Runner, ports svclib.Ports, servicesErrCh chan error) error {
	mux := http.NewServeMux()
	handle(ctx, mux, r, servicesErrCh, "GET /", handleUI)
	handle(ctx, mux, r, servicesErrCh, "GET /v0/log", handleLog)
	handle(ctx, mux, r, servicesErrCh, "GET /v0/healthcheck", handleHealthCheck)
	handle(ctx, mux, r, servicesErrCh, "GET /v0/start", handleStart)
	handle(ctx, mux, r, servicesErrCh, "GET /v0/kill", handleKill)
	handle(ctx, mux, r, servicesErrCh, "GET /v0/wait", handleWait)
	handle(ctx, mux, r, servicesErrCh, "GET /v0/port", portHandler{ports}.handle)
	return http.Serve(listener, mux)
}
