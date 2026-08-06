package svclib

import "encoding/json"

type Ports map[string]string

func (p Ports) Set(service string, port string) {
	p[service] = port
}

func (p Ports) Marshal() ([]byte, error) {
	return json.Marshal(p)
}

func (p *Ports) Unmarshal(data []byte) error {
	return json.Unmarshal(data, p)
}

// BindingInfo is the rich, per-port description exported through ITEST_PORTS_MAP
// and ITEST_SERVICES_MAP.
type BindingInfo struct {
	// Origin is "<domain>:<port>".
	Origin string `json:"origin"`
	Domain string `json:"domain"`
	Port   string `json:"port"`
}

// PortsMap is keyed by port target label (and its aliases).
type PortsMap map[string]BindingInfo

func (m PortsMap) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// Port returns just the port number bound under the given label (or alias), or "" if unbound.
func (m PortsMap) Port(label string) string {
	return m[label].Port
}

// AssignedPorts derives the legacy label->port string map (used for the ASSIGNED_PORTS
// env var and the /v0/port endpoint) from the rich binding info.
func (m PortsMap) AssignedPorts() Ports {
	ports := make(Ports, len(m))
	for label, info := range m {
		ports[label] = info.Port
	}
	return ports
}

// ServicesMap is keyed by service target label, then by port name (and aliases).
type ServicesMap map[string]map[string]BindingInfo

func (m ServicesMap) Set(service, portName string, info BindingInfo) {
	inner, ok := m[service]
	if !ok {
		inner = map[string]BindingInfo{}
		m[service] = inner
	}
	inner[portName] = info
}

func (m ServicesMap) Marshal() ([]byte, error) {
	return json.Marshal(m)
}
