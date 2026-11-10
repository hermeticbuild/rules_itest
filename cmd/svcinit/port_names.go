package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"rules_itest/svclib"
)

func qualifiedPortName(label, name string) string {
	if name == "" {
		return label
	}
	return label + "." + name
}

func assignPortAliases(ports svclib.Ports, serviceSpecs map[string]svclib.ServiceSpec) error {
	aliases := make(map[string]string)
	for label, spec := range serviceSpecs {
		for name, target := range spec.PortAliases {
			key := qualifiedPortName(label, name)
			_, allocated := ports[key]
			_, aliased := aliases[key]
			if allocated || aliased {
				return fmt.Errorf("duplicate port name %q", key)
			}
			aliases[key] = target
		}
	}

	// Consume each chain once. A name with no remaining target is either
	// missing from the specs or already on the current path.
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		var path []string
		for {
			if port, ok := ports[name]; ok {
				for _, alias := range path {
					ports[alias] = port
				}
				break
			}
			target, ok := aliases[name]
			if !ok {
				if i := slices.Index(path, name); i >= 0 {
					return fmt.Errorf("port alias cycle: %s", strings.Join(append(path[i:], name), " -> "))
				}
				return fmt.Errorf("unknown port alias target %q", name)
			}
			path = append(path, name)
			delete(aliases, name)
			name = target
		}
	}
	return nil
}
