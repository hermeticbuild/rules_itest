package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/hermeticbuild/rules_itest/tests/svcctl"
)

type bindingInfo struct {
	Origin string `json:"origin"`
	Hostname string `json:"hostname"`
	Port   string `json:"port"`
}

func loadPortsMap(t *testing.T) map[string]bindingInfo {
	t.Helper()
	raw := os.Getenv("ITEST_PORTS_MAP")
	if raw == "" {
		t.Fatal("ITEST_PORTS_MAP is not set")
	}
	out := map[string]bindingInfo{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("failed to parse ITEST_PORTS_MAP %q: %v", raw, err)
	}
	return out
}

func loadServicesMap(t *testing.T) map[string]map[string]bindingInfo {
	t.Helper()
	raw := os.Getenv("ITEST_SERVICES_MAP")
	if raw == "" {
		t.Fatal("ITEST_SERVICES_MAP is not set")
	}
	out := map[string]map[string]bindingInfo{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("failed to parse ITEST_SERVICES_MAP %q: %v", raw, err)
	}
	return out
}

func TestPortsMap(t *testing.T) {
	target := os.Getenv("EXPECT_PORT_TARGET")
	wantHostname := os.Getenv("EXPECT_HOSTNAME")
	wantPort := os.Getenv("EXPECT_PORT") // optional exact match

	portsMap := loadPortsMap(t)
	info, ok := portsMap[target]
	if !ok {
		t.Fatalf("ITEST_PORTS_MAP missing port target %q; got %v", target, portsMap)
	}
	if info.Hostname != wantHostname {
		t.Errorf("port %q hostname = %q, want %q", target, info.Hostname, wantHostname)
	}
	if info.Port == "" {
		t.Errorf("port %q has empty port", target)
	}
	if wantPort != "" && info.Port != wantPort {
		t.Errorf("port %q port = %q, want %q", target, info.Port, wantPort)
	}
	if want := info.Hostname + ":" + info.Port; info.Origin != want {
		t.Errorf("port %q origin = %q, want %q", target, info.Origin, want)
	}
}

func TestServicesMap(t *testing.T) {
	service := os.Getenv("EXPECT_SERVICE")
	portName := os.Getenv("EXPECT_PORT_NAME")
	wantHostname := os.Getenv("EXPECT_HOSTNAME")

	servicesMap := loadServicesMap(t)
	ports, ok := servicesMap[service]
	if !ok {
		t.Fatalf("ITEST_SERVICES_MAP missing service %q; got %v", service, servicesMap)
	}
	info, ok := ports[portName]
	if !ok {
		t.Fatalf("service %q missing port name %q; got %v", service, portName, ports)
	}
	if info.Hostname != wantHostname {
		t.Errorf("service %q port %q hostname = %q, want %q", service, portName, info.Hostname, wantHostname)
	}
	if info.Port == "" {
		t.Errorf("service %q port %q has empty port", service, portName)
	}
}

func TestSvcctlListAll(t *testing.T) {
	svcctlPort := os.Getenv("SVCCTL_PORT")
	if svcctlPort == "" {
		t.Fatal("SVCCTL_PORT not set")
	}
	client := svcctl.NewSvcctlClient("http://127.0.0.1:"+svcctlPort, http.DefaultClient)

	target := os.Getenv("EXPECT_PORT_TARGET")
	service := os.Getenv("EXPECT_SERVICE")
	portName := os.Getenv("EXPECT_PORT_NAME")

	// /v0/ports should agree with the env-provided ITEST_PORTS_MAP.
	envPorts := loadPortsMap(t)
	apiPorts, err := client.Ports(context.Background())
	if err != nil {
		t.Fatalf("GET /v0/ports failed: %v", err)
	}
	if apiPorts[target].Port != envPorts[target].Port {
		t.Errorf("/v0/ports port for %q = %q, want %q", target, apiPorts[target].Port, envPorts[target].Port)
	}
	if apiPorts[target].Hostname != envPorts[target].Hostname {
		t.Errorf("/v0/ports hostname for %q = %q, want %q", target, apiPorts[target].Hostname, envPorts[target].Hostname)
	}

	// /v0/services should expose the service's port.
	apiServices, err := client.Services(context.Background())
	if err != nil {
		t.Fatalf("GET /v0/services failed: %v", err)
	}
	svc, ok := apiServices[service]
	if !ok {
		t.Fatalf("/v0/services missing service %q; got %v", service, apiServices)
	}
	if _, ok := svc[portName]; !ok {
		t.Fatalf("/v0/services service %q missing port %q; got %v", service, portName, svc)
	}
}
