package runner

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"rules_itest/svclib"
)

func TestCommandHealthCheckBoundsInheritedOutputPipeWait(t *testing.T) {
	t.Setenv("RULES_ITEST_HEALTHCHECK_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	service := &ServiceInstance{
		VersionedServiceSpec: svclib.VersionedServiceSpec{ServiceSpec: svclib.ServiceSpec{
			Type:            "service",
			Label:           "//example:service",
			HealthCheck:     executable,
			HealthCheckArgs: []string{"-test.run=TestCommandHealthCheckHelperProcess"},
		}},
		cmd: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}},
	}

	start := time.Now()
	if !service.HealthCheck(context.Background(), 0) {
		t.Fatal("HealthCheck() = false, want successful health-check exit to remain successful")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("HealthCheck() took %s, want less than 1s", elapsed)
	}
}

func TestCommandHealthCheckHelperProcess(t *testing.T) {
	if os.Getenv("RULES_ITEST_HEALTHCHECK_HELPER") != "1" {
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestCommandHealthCheckDescendantProcess")
	cmd.Env = append(os.Environ(), "RULES_ITEST_HEALTHCHECK_DESCENDANT=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestCommandHealthCheckDescendantProcess(t *testing.T) {
	if os.Getenv("RULES_ITEST_HEALTHCHECK_DESCENDANT") != "1" {
		return
	}
	time.Sleep(2 * time.Second)
}
