package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperExitProcess is not a real test: it is the portable failing child
// process that TestWaitUntilHealthyErrors launches by re-running this binary.
func TestHelperExitProcess(t *testing.T) {
	if os.Getenv("RULES_ITEST_HELPER_PROCESS") != "1" {
		t.Skip("helper process only")
	}
	os.Exit(1)
}

func TestWaitUntilHealthyErrors(t *testing.T) {
	const label = "//example:server"

	exitCmd := exec.Command(os.Args[0], "-test.run=^TestHelperExitProcess$")
	exitCmd.Env = append(os.Environ(), "RULES_ITEST_HELPER_PROCESS=1")
	exitErr := exitCmd.Run()
	var exitErrType *exec.ExitError
	if !errors.As(exitErr, &exitErrType) {
		t.Fatalf("cmd.Run() error = %v, want *exec.ExitError", exitErr)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	taskErr := errors.New("task failed")
	runErr := errors.New("service failed")

	tests := []struct {
		name    string
		typ     string
		service ServiceInstance
		ctx     context.Context
		wantIs  error
		wantMsg string
	}{
		{
			name:    "task failure",
			typ:     "task",
			service: ServiceInstance{waitErrFn: func() error { return taskErr }},
			wantIs:  taskErr,
			wantMsg: "exited with error",
		},
		{
			name:    "recorded service error",
			typ:     "service",
			service: ServiceInstance{runErr: runErr},
			wantIs:  runErr,
			wantMsg: "exited with error",
		},
		{
			name:    "exit before runErr is recorded",
			typ:     "service",
			service: ServiceInstance{cmd: exitCmd, waitErrFn: func() error { return exitErr }, done: true},
			wantIs:  exitErr,
			wantMsg: "exited before becoming healthy",
		},
		{
			name:    "context expiry",
			typ:     "service",
			service: ServiceInstance{cmd: &exec.Cmd{}},
			ctx:     canceled,
			wantIs:  context.Canceled,
			wantMsg: "never became healthy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &tt.service
			service.Type = tt.typ
			service.Label = label
			service.HealthCheckInterval = "1ms"
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			err := service.WaitUntilHealthy(ctx)
			if err == nil {
				t.Fatal("WaitUntilHealthy() error = nil")
			}
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("error = %v, want wrapped %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), label) || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want label %q and %q", err, label, tt.wantMsg)
			}
		})
	}
}
