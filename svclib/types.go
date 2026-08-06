package svclib

import "rules_itest/logger"

// PortBinding describes a single port that a service binds (internal) or points
// at (external). It is created by Starlark and carries the canonical port target
// label along with any backwards-compatible aliases that must resolve to the same
// port in the various port maps.
type PortBinding struct {
	// Target is the canonical port target label (e.g. an `itest_port` label, or for
	// legacy services the service's own label / `label.port_name`).
	Target string `json:"target"`
	// Name is the port name used as the inner key in ITEST_SERVICES_MAP.
	Name string `json:"name"`
	// Aliases are additional keys that must resolve to the same port (for example,
	// user-declared `itest_port` aliases) for backwards compatibility.
	Aliases []string `json:"aliases"`
	// Value is the desired port. For internal services "0" means autoassign. For
	// external services it is the literal port number on the remote host.
	Value string `json:"value"`
}

// Created by Starlark
type ServiceSpec struct {
	// Type can be "service", "task", "group", or "external_service".
	Type                    string            `json:"type"`
	Label                   string            `json:"label"`
	Args                    []string          `json:"args"`
	Env                     map[string]string `json:"env"`
	Exe                     string            `json:"exe"`
	HttpHealthCheckAddress  string            `json:"http_health_check_address"`
	ExpectedStartDuration   string            `json:"expected_start_duration"`
	HealthCheck             string            `json:"health_check"`
	HealthCheckLabel        string            `json:"health_check_label"`
	HealthCheckArgs         []string          `json:"health_check_args"`
	HealthCheckInterval     string            `json:"health_check_interval"`
	HealthCheckTimeout      string            `json:"health_check_timeout"`
	VersionFile             string            `json:"version_file"`
	Deps                    []string          `json:"deps"`
	Port                    string            `json:"port"`
	AutoassignPort          bool              `json:"autoassign_port"`
	SoReuseportAware        bool              `json:"so_reuseport_aware"`
	NamedPorts              map[string]string `json:"named_ports"`
	HotReloadable           bool              `json:"hot_reloadable"`
	PortAliases             map[string]string `json:"port_aliases"`
	ShutdownSignal          string            `json:"shutdown_signal"`
	ShutdownTimeout         string            `json:"shutdown_timeout"`
	EnforceForcefulShutdown bool              `json:"enforce_graceful_shutdown"`
	Deferred                bool              `json:"deferred"`
	// Domain is the host that this service's ports are reachable on. Internal
	// services default to "127.0.0.1"; external services set it to their FQDN.
	Domain string `json:"domain"`
	// PortBindings is the canonical, target-keyed description of the ports owned by
	// this service. It supersedes Port/NamedPorts (which are retained for
	// backwards compatibility).
	PortBindings []PortBinding `json:"port_bindings"`
}

// Our internal representation.
type VersionedServiceSpec struct {
	ServiceSpec
	Version string
	Color   string
}

func (v VersionedServiceSpec) Colorize(label string) string {
	return v.Color + label + logger.Reset
}
