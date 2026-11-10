package main

import (
	"maps"
	"os"
	"testing"

	"rules_itest/svclib"
)

func TestNestedGroupPortAliases(t *testing.T) {
	ports := svclib.Ports{"svc.http": "8020"}
	err := assignPortAliases(ports, map[string]svclib.ServiceSpec{
		"consumer": {PortAliases: map[string]string{"": "inner.public", "http": "inner.public"}},
		"inner":    {PortAliases: map[string]string{"public": "svc.http"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := svclib.Ports{"svc.http": "8020", "inner.public": "8020", "consumer": "8020", "consumer.http": "8020"}
	if !maps.Equal(ports, want) {
		t.Fatalf("published ports = %v, want %v", ports, want)
	}
}

func TestInvalidPortNames(t *testing.T) {
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
		{"two allocations", `duplicate port name "svc.http"`, map[string]svclib.ServiceSpec{
			"svc":      {NamedPorts: map[string]string{"http": "0"}},
			"svc.http": {AutoassignPort: true, Port: "0"},
		}},
		{"two default port requests", `duplicate port name "svc"`, map[string]svclib.ServiceSpec{
			"svc": {AutoassignPort: true, Port: "0", NamedPorts: map[string]string{"": "0"}},
		}},
		{"alias and allocation", `duplicate port name "group.http"`, map[string]svclib.ServiceSpec{
			"group":      {PortAliases: map[string]string{"http": "group.http"}},
			"group.http": {AutoassignPort: true, Port: "0"},
		}},
		{"two aliases for the same target", `duplicate port name "group.http"`, map[string]svclib.ServiceSpec{
			"svc":        {AutoassignPort: true, Port: "0"},
			"group":      {PortAliases: map[string]string{"http": "svc"}},
			"group.http": {PortAliases: map[string]string{"": "svc"}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ports, held, control, err := assignPorts(test.specs, "127.0.0.1:0")
			defer closeReservedPorts(held)
			if control != nil {
				defer control.Close()
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("assignPorts returned ports %v, error %v; want %q", ports, err, test.want)
			}
			if got := os.Getenv("ASSIGNED_PORTS"); got != "unchanged" {
				t.Fatalf("published ASSIGNED_PORTS after failed assignment: %s", got)
			}
		})
	}
}
