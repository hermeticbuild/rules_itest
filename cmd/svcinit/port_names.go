package main

import "rules_itest/svclib"

func assignPortAliases(ports svclib.Ports, serviceSpecs map[string]svclib.ServiceSpec) {
	for label, spec := range serviceSpecs {
		for portName, aliasedTo := range spec.PortAliases {
			qualifiedPortName := label
			if portName != "" {
				qualifiedPortName += "." + portName
			}

			ports.Set(qualifiedPortName, ports[aliasedTo])

			{
				// TODO(zbarsky): Clean this up after April 2026
				qualifiedPortName := label
				if portName != "" {
					qualifiedPortName += ":" + portName
				}

				ports.Set(qualifiedPortName, ports[aliasedTo])
			}
		}
	}
}
