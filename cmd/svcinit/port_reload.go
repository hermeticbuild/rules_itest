package main

import (
	"fmt"
	"maps"
	"strconv"

	"rules_itest/svclib"
)

func namedPorts(spec svclib.ServiceSpec) map[string]string {
	if !spec.AutoassignPort {
		return spec.NamedPorts
	}
	ports := make(map[string]string, len(spec.NamedPorts)+1)
	maps.Copy(ports, spec.NamedPorts)
	ports[""] = spec.Port
	return ports
}

// The manager and its clients keep the port table published at startup.
func checkPortReload(active, next map[string]svclib.ServiceSpec, ports svclib.Ports) error {
	restart := func(name string) error {
		return fmt.Errorf("port requirements changed for %s; restart the service manager to reassign ports", name)
	}
	assigned := make(map[portName]string)
	for label, spec := range next {
		previous := active[label]
		owned := namedPorts(previous)
		for name, request := range namedPorts(spec) {
			key := portName{label, name}
			public := key.String()
			want, err := strconv.ParseUint(request, 10, 16)
			if err != nil {
				return fmt.Errorf("invalid port %q for %s: %w", request, public, err)
			}
			// A public alias is not a socket assigned to this service.
			number := ports[public]
			if _, ok := owned[name]; !ok || number == "" || previous.SoReuseportAware != spec.SoReuseportAware {
				return restart(public)
			}
			if want != 0 && strconv.FormatUint(want, 10) != number {
				return restart(public)
			}
			assigned[key] = number
		}
	}
	for label, spec := range active {
		for name := range namedPorts(spec) {
			key := portName{label, name}
			if _, ok := assigned[key]; !ok {
				return restart(key.String())
			}
		}
	}
	current, err := publicPorts(assigned, next)
	if err != nil {
		return err
	}
	if !maps.Equal(current, ports) {
		return fmt.Errorf("published port names or numbers changed; restart the service manager to reassign ports")
	}
	return nil
}
