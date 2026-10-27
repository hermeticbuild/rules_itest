package svclib

import "rules_itest/logger"

// Created by Starlark
type ServiceSpec struct {
	// Type can be "service", "task", or "group".
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
	PortFamily              string            `json:"port_family"`
	SoReuseportAware        bool              `json:"so_reuseport_aware"`
	NamedPorts              map[string]string `json:"named_ports"`
	HotReloadable           bool              `json:"hot_reloadable"`
	PortAliases             map[string]string `json:"port_aliases"`
	ShutdownSignal          string            `json:"shutdown_signal"`
	ShutdownTimeout         string            `json:"shutdown_timeout"`
	EnforceForcefulShutdown bool              `json:"enforce_graceful_shutdown"`
	Deferred                bool              `json:"deferred"`
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
