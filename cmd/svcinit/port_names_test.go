package main

import (
	"maps"
	"os"
	"strings"
	"testing"

	"rules_itest/svclib"
)

func TestNestedGroupPortAliases(t *testing.T) {
	ports, err := publicPorts(map[portName]string{{"svc", "http"}: "8020"}, map[string]svclib.ServiceSpec{
		"outer":    {PortAliases: map[string]string{"": "inner.public"}},
		"inner":    {PortAliases: map[string]string{"public": "svc.http"}},
		"svc.http": {PortAliases: map[string]string{"": "svc.http"}}, // Also names the physical port.
	})
	if err != nil {
		t.Fatal(err)
	}
	want := svclib.Ports{"outer": "8020", "inner.public": "8020", "svc.http": "8020"}
	if !maps.Equal(ports, want) {
		t.Fatalf("published ports = %v, want %v", ports, want)
	}
}

func TestInvalidGroupPortAliases(t *testing.T) {
	t.Setenv("ASSIGNED_PORTS", "unchanged")
	for _, test := range []struct {
		name, want string
		specs      map[string]svclib.ServiceSpec
	}{
		{"missing target", `unknown port alias target "missing"`, map[string]svclib.ServiceSpec{
			"outer": {PortAliases: map[string]string{"": "inner"}},
			"inner": {PortAliases: map[string]string{"": "missing"}},
		}},
		{"cycle", "port alias cycle: inner -> outer -> inner", map[string]svclib.ServiceSpec{
			"consumer": {PortAliases: map[string]string{"": "inner"}},
			"inner":    {PortAliases: map[string]string{"": "outer"}},
			"outer":    {PortAliases: map[string]string{"": "inner"}},
		}},
		{"ambiguous name", "conflicting assignments for svc.http:", map[string]svclib.ServiceSpec{
			"svc":      {NamedPorts: map[string]string{"http": "0"}},
			"svc.http": {AutoassignPort: true, Port: "0", NamedPorts: map[string]string{}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ports, held, err := assignPorts(test.specs)
			if err == nil {
				closeReservedPorts(held)
				t.Fatalf("published invalid port assignments: %v", ports)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
			if got := os.Getenv("ASSIGNED_PORTS"); got != "unchanged" {
				t.Fatalf("published ASSIGNED_PORTS after failed assignment: %s", got)
			}
		})
	}
}
