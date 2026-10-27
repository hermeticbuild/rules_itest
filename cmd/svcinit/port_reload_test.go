package main

import (
	"maps"
	"testing"

	"rules_itest/svclib"
)

func TestPortReload(t *testing.T) {
	svc := svclib.ServiceSpec{AutoassignPort: true, Port: "0", NamedPorts: map[string]string{"http": "0"}}
	startup := map[string]svclib.ServiceSpec{
		"svc":   svc,
		"group": {PortAliases: map[string]string{"": "svc.http"}},
		"outer": {PortAliases: map[string]string{"": "group"}},
		"empty": {},
	}
	ports := svclib.Ports{"svc": "8000", "svc.http": "8001", "group": "8001", "outer": "8001"}
	check := func(description string, next map[string]svclib.ServiceSpec, want bool) {
		t.Helper()
		if err := checkPortReload(startup, next, ports); (err == nil) != want {
			t.Errorf("%s: got %v, want compatible=%t", description, err, want)
		}
	}
	replace := func(label string, spec svclib.ServiceSpec) map[string]svclib.ServiceSpec {
		next := maps.Clone(startup)
		next[label] = spec
		return next
	}

	check("unchanged", startup, true)
	changed := svc
	changed.Port, changed.NamedPorts = "8000", map[string]string{"http": "8001"}
	check("pin to assigned numbers", replace("svc", changed), true)
	changed.Port = "8001"
	check("change fixed port", replace("svc", changed), false)
	changed = svc
	changed.NamedPorts = nil
	check("remove named port", replace("svc", changed), false)
	changed.NamedPorts = map[string]string{"http": "0", "admin": "0"}
	check("add named port", replace("svc", changed), false)
	changed = svc
	changed.SoReuseportAware = true
	check("change reservation mode", replace("svc", changed), false)

	check("retarget outer alias to the same port", replace("outer", svclib.ServiceSpec{PortAliases: map[string]string{"": "svc.http"}}), true)
	check("retarget alias", replace("group", svclib.ServiceSpec{PortAliases: map[string]string{"": "svc"}}), false)
	check("allocate an alias as a service port", replace("group", svclib.ServiceSpec{AutoassignPort: true, Port: "0"}), false)
	next := maps.Clone(startup)
	delete(next, "outer")
	check("remove only an alias", next, false)
	next = maps.Clone(startup)
	delete(next, "empty")
	check("remove portless service", next, true)
}

func TestReloadKeepsPhysicalPortOwners(t *testing.T) {
	owner := svclib.ServiceSpec{AutoassignPort: true, Port: "0"}
	startup := map[string]svclib.ServiceSpec{"first": owner, "second": owner}
	ports := svclib.Ports{"first": "8000", "second": "8000"}
	next := maps.Clone(startup)
	next["first"] = svclib.ServiceSpec{PortAliases: map[string]string{"": "second"}}
	if err := checkPortReload(startup, next, ports); err == nil {
		t.Fatal("replaced a physical port with an alias without changing the published table")
	}
}

func TestReloadPortFamily(t *testing.T) {
	specs := func(family string, reusable bool) map[string]svclib.ServiceSpec {
		return map[string]svclib.ServiceSpec{"svc": {AutoassignPort: true, Port: "0", PortFamily: family, SoReuseportAware: reusable}}
	}
	check := func(active, next map[string]svclib.ServiceSpec, compatible bool) {
		t.Helper()
		if err := checkPortReload(active, next, svclib.Ports{"svc": "8000"}); (err == nil) != compatible {
			t.Fatalf("reload from %+v to %+v: got %v, want compatible=%t", active["svc"], next["svc"], err, compatible)
		}
	}

	active := specs("dual-stack", false)
	check(active, specs("ipv6", false), true)
	next := specs("ipv4", false)
	check(active, next, true)
	active = next
	check(active, specs("dual-stack", false), false)
	check(active, specs("ipv6", false), false)
	check(specs("", false), active, true)
	reusable := specs("dual-stack", true)
	check(reusable, reusable, true)
	check(reusable, specs("ipv4", true), false)
}
