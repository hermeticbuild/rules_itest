package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"rules_itest/svclib"
)

type portName struct{ service, name string }

func (n portName) String() string {
	if n.name == "" {
		return n.service
	}
	return n.service + "." + n.name
}

func publicPorts(assigned map[portName]string, specs map[string]svclib.ServiceSpec) (svclib.Ports, error) {
	// Each published number wakes the aliases that depend on it.
	waiting := make(map[string][]string)
	for label, spec := range specs {
		for name, target := range spec.PortAliases {
			waiting[target] = append(waiting[target], (portName{label, name}).String())
		}
	}

	type binding struct{ name, number string }
	ready := make([]binding, 0, len(assigned))
	for name, number := range assigned {
		ready = append(ready, binding{name.String(), number})
	}
	ports := make(svclib.Ports, len(ready))
	for i := 0; i < len(ready); i++ {
		b := ready[i]
		if existing, ok := ports[b.name]; ok && existing != b.number {
			return nil, fmt.Errorf("conflicting assignments for %s: %s and %s", b.name, existing, b.number)
		}
		ports[b.name] = b.number
		for _, alias := range waiting[b.name] {
			ready = append(ready, binding{alias, b.number})
		}
		delete(waiting, b.name)
	}
	if len(waiting) > 0 {
		return nil, unresolvedAlias(waiting)
	}
	return ports, nil
}

func unresolvedAlias(waiting map[string][]string) error {
	// Follow an unresolved chain to the missing target or the cycle itself.
	targets := slices.Sorted(maps.Keys(waiting))
	next := make(map[string]string)
	for _, target := range targets {
		for _, alias := range waiting[target] {
			next[alias] = target
		}
	}
	var path []string
	seen := make(map[string]int)
	for name := targets[0]; ; name = next[name] {
		if i, ok := seen[name]; ok {
			return fmt.Errorf("port alias cycle: %s", strings.Join(append(path[i:], name), " -> "))
		}
		if _, ok := next[name]; !ok {
			return fmt.Errorf("unknown port alias target %q", name)
		}
		seen[name] = len(path)
		path = append(path, name)
	}
}
