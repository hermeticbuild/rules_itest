package runner

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"rules_itest/svclib"
)

func TestStartAllSkipsAlreadyRunningServices(t *testing.T) {
	service := &ServiceInstance{
		VersionedServiceSpec: svclib.VersionedServiceSpec{
			ServiceSpec: svclib.ServiceSpec{
				Type:  "service",
				Label: "//:unchanged",
			},
		},
		cmd: &exec.Cmd{
			Args:    []string{"unchanged"},
			Process: &os.Process{Pid: 1},
		},
	}
	runner := &Runner{
		ctx: context.Background(),
		serviceInstances: map[string]*ServiceInstance{
			service.Label: service,
		},
	}

	if _, err := runner.StartAll(make(chan error, 1)); err != nil {
		t.Fatalf("StartAll() returned an error for an already-running service: %v", err)
	}
}

func TestUpdateSpecsAndRestartRemovesGroup(t *testing.T) {
	service := svclib.ServiceSpec{Type: "service", Label: "//:service", Exe: os.Args[0]}
	group := svclib.ServiceSpec{Type: "group", Label: "//:group", Deps: []string{service.Label}}
	r, err := New(context.Background(), ServiceSpecs{
		service.Label: {ServiceSpec: service},
		group.Label:   {ServiceSpec: group},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.UpdateSpecsAndRestart(ServiceSpecs{}, make(chan error, 1), nil); err != nil {
		t.Fatal(err)
	}
	if instance := r.GetInstance(group.Label); instance != nil {
		t.Fatalf("GetInstance(%q) = %v, want nil", group.Label, instance)
	}
}
