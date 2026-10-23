package main

import (
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"rules_itest/svclib"
)

func TestServicePortRequestsFixedBeforeDynamic(t *testing.T) {
	got := servicePortRequests(map[string]svclib.ServiceSpec{
		"alpha": {
			AutoassignPort: true,
			Port:           "+",
			NamedPorts:     map[string]string{"": "34567", "fixed": "12345"},
		},
		"beta": {
			AutoassignPort:   true,
			Port:             "23456",
			NamedPorts:       map[string]string{"dynamic": "+0"},
			SoReuseportAware: true,
		},
	})
	want := []portRequest{
		{label: "alpha", name: "fixed", port: "12345"},
		{label: "beta", port: "23456", reusable: true},
		{label: "alpha", port: "+"},
		{label: "beta", name: "dynamic", port: "+0", reusable: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got requests %v, want %v", got, want)
	}
	// The order within each phase is unspecified.
	for _, phase := range [][]portRequest{got[:2], got[2:]} {
		slices.SortFunc(phase, func(a, b portRequest) int { return strings.Compare(a.label, b.label) })
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got requests %v, want %v", got, want)
	}
}

func TestControlListenerOpensBeforeServicePortsAreReleased(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().(*net.TCPAddr)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ASSIGNED_PORTS", "previous")
	_, reserved, control, err := assignPorts(map[string]svclib.ServiceSpec{
		"service": {AutoassignPort: true, Port: strconv.Itoa(addr.Port)},
	}, addr.String())
	defer closeReservedPorts(reserved)
	if control != nil {
		defer control.Close()
	}
	if err == nil || !strings.HasPrefix(err.Error(), "listen for svcctl: ") {
		t.Fatalf("expected the control listener to conflict with the service reservation, got %v", err)
	}
	if got := os.Getenv("ASSIGNED_PORTS"); got != "previous" {
		t.Fatalf("failed allocation changed ASSIGNED_PORTS to %q", got)
	}
	listener, err := net.Listen("tcp", addr.String())
	if err != nil {
		t.Fatalf("service reservation was not released: %v", err)
	}
	listener.Close()
}
