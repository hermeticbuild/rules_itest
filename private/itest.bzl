"""
# Rules for running services in integration tests.

This ruleset supports [ibazel](https://github.com/bazelbuild/bazel-watcher) when using `bazel run`.
As a UX optimization, the service manager is able to restart only the modified services, instead of all services,
when it receives the reload notification from ibazel. This capability depends on a cache-busting input, so it is hidden
behind an an extra CLI flag, like so:
```
.bazelrc

build:enable-reload --@rules_itest//:enable_per_service_reload
fetch:enable-reload --@rules_itest//:enable_per_service_reload
query:enable-reload --@rules_itest//:enable_per_service_reload
```

`ibazel run --config enable-reload //path/to:target`

In addition, if the `hot_reloadable` attribute is set on an `itest_service`, the service manager will
forward the ibazel hot-reload notification over stdin instead of restarting the service.

# Service control

The service manager exposes a HTTP server on `http://127.0.0.1:{SVCCTL_PORT}`. It can be used to
start / stop services during a test run. There are currently 7 API endpoints available.
All of them are GET requests:

1. `/v0/healthcheck?service={label}`: Returns 200 if the service is healthy, 503 otherwise.
2. `/v0/start?service={label}`: Starts the service if it is not already running.
3. `/v0/kill?service={label}[&signal={signal}]`: Send kill signal to the service if it is running.
   You can optionally specify the signal to send to the service (valid values: SIGTERM and SIGKILL).
4. `/v0/wait?service={label}`: Wait for the service to exit and returns the exit code in the body.
5. `/v0/port?service={label}`: Returns the assigned port for the given label. May be a named port.
6. `/v0/ports`: Returns the full `ITEST_PORTS_MAP` as JSON (every port target/alias -> binding info).
7. `/v0/services`: Returns the full `ITEST_SERVICES_MAP` as JSON (every service -> port name -> binding info).

In `bazel run` mode, the service manager will write the value of `SVCCTL_PORT` to `/tmp/svcctl_port`.
This can be used in conjunction with the `/v0/port` API to let other tools interact with the managed services.

# Ports, hostnames, and external services

Ports can be declared as first-class targets with `itest_port`. A port is a handle whose value is an `int`
build-setting flag (`0` = autoassign); the host/domain is supplied by the internal or external service that
binds it. The value can be pinned from the command line via `--//pkg:my_port=8080`. A given port may only
ever be bound once.

`itest_external_service` points at a production or production-like instance reachable at a fixed FQDN.
It provides the same information as an `itest_service`, so it can be swapped in for a local service using
Bazel `select()` to run a test suite against production-like instances. External services are never started
or stopped by the service manager; if a health check is configured, it is run to verify reachability.

Two environment variables describe all bound ports/services (both internal and external). They are injected
into the test binary and every child service, and are also available through the `/v0/ports` and `/v0/services`
control APIs:

- `ITEST_PORTS_MAP`: a JSON object keyed by port target label (and aliases):
  `{"@@//pkg:my_port": {"origin": "127.0.0.1:54321", "domain": "127.0.0.1", "port": "54321"}, ...}`
- `ITEST_SERVICES_MAP`: a JSON object keyed by service target label, then by port name (and aliases):
  `{"@@//pkg:my_service": {"http": {"origin": "...", "domain": "...", "port": "..."}}, ...}`

The legacy `ASSIGNED_PORTS` env var, the `GET_ASSIGNED_PORT_BIN` helper, and the `/v0/port` endpoint all
continue to work as before.
"""

load("@bazel_lib//lib:paths.bzl", "to_rlocation_path")
load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

_ServiceGroupInfo = provider(
    doc = "Info about a service group",
    fields = {
        "deferred": "Flag if this service/task/group should be deferred or not",
        "services": "Dict of services/tasks",
    },
)

_PortInfo = provider(
    doc = "A handle for a port that can be bound by an internal or external service.",
    fields = {
        "label": "The canonical (fully-qualified) label of the port target.",
        "aliases": "Additional keys that must resolve to the same port for backwards compatibility.",
    },
)

def _collect_services(deps):
    services = {}
    for dep in deps:
        services |= dep[_ServiceGroupInfo].services
    return services

def _validate_unique_port_bindings(services):
    """Ensures every port target is bound by at most one service."""
    seen = {}
    for label, service in services.items():
        bindings = getattr(service, "port_bindings", None)
        if not bindings:
            continue
        for binding in bindings:
            if binding.target in seen and seen[binding.target] != label:
                fail("Port %s is bound by multiple services: %s and %s. A port may only be bound once." % (
                    binding.target,
                    seen[binding.target],
                    label,
                ))
            seen[binding.target] = label

def _port_binding(target, name, aliases, value):
    """Constructs a single canonical port binding. Shared by internal and external services."""
    return struct(
        target = target,
        name = name,
        aliases = aliases,
        value = value,
    )

def _bind_port_targets(ctx, value_fn):
    """Builds bindings for each `itest_port` target in `ctx.attr.ports`.

    `value_fn(port_target, name)` returns the desired value string for that binding, letting
    internal services read it from the port flag while external services read it from `port_numbers`.
    """
    return [
        _port_binding(
            port_target[_PortInfo].label,
            name,
            port_target[_PortInfo].aliases,
            value_fn(port_target, name),
        )
        for port_target, name in ctx.attr.ports.items()
    ]

def _compute_port_bindings(ctx):
    """Builds the canonical, target-keyed list of port bindings for an internal service.

    Legacy `autoassign_port` / `named_ports` are translated into bindings keyed by the
    service's own label (so historical references keep working), and any explicit
    `itest_port` targets in `ports` are bound using their `_PortInfo`.
    """
    bindings = []
    label = str(ctx.label)

    if ctx.attr.autoassign_port:
        bindings.append(_port_binding(label, "", [], str(ctx.attr.port[BuildSettingInfo].value)))

    for port_flag, name in ctx.attr.named_ports.items():
        bindings.append(_port_binding(label + "." + name, name, [], str(port_flag[BuildSettingInfo].value)))

    bindings += _bind_port_targets(ctx, lambda port_target, _name: str(port_target[BuildSettingInfo].value))

    return bindings

def _run_environment(ctx, service_specs_file):
    return {
        # Flags
        "SVCINIT_ALLOW_CONFIGURING_TMPDIR": str(ctx.attr._allow_configuring_tmpdir[BuildSettingInfo].value),
        "SVCINIT_ENABLE_PER_SERVICE_RELOAD": str(ctx.attr._enable_per_service_reload[BuildSettingInfo].value),
        "SVCINIT_KEEP_SERVICES_UP": str(ctx.attr._keep_services_up[BuildSettingInfo].value),
        "SVCINIT_TERSE_OUTPUT": str(ctx.attr._terse_svcinit_output[BuildSettingInfo].value),

        # Specs
        "SVCINIT_SERVICE_SPECS_RLOCATION_PATH": to_rlocation_path(ctx, service_specs_file),
    }

def _services_runfiles(ctx, services_attr_name = "services"):
    return [
        service.default_runfiles
        for service in getattr(ctx.attr, services_attr_name)
    ] + [
        ctx.attr._svcinit.default_runfiles,
    ]

_svcinit_attrs = {
    "_svcinit": attr.label(
        default = "//cmd/svcinit",
        executable = True,
        cfg = "target",
    ),
    "_enable_per_service_reload": attr.label(
        default = "//:enable_per_service_reload",
    ),
    "_allow_configuring_tmpdir": attr.label(
        default = "//:allow_configuring_tmpdir",
    ),
    "_keep_services_up": attr.label(
        default = "//:keep_services_up",
    ),
    "_terse_svcinit_output": attr.label(
        default = "//:terse_svcinit_output",
    ),
}

_itest_binary_attrs = {
    "deferred": attr.bool(
        doc = """If set, the service/task will not be started on boot up. It can be started using the service manager's control API.""",
    ),
    "exe": attr.label(
        mandatory = True,
        executable = True,
        cfg = "target",
        doc = "The binary target to run.",
    ),
    "env": attr.string_dict(
        doc = "The service manager will merge these variables into the environment when spawning the underlying binary.",
    ),
    "data": attr.label_list(allow_files = True),
    "deps": attr.label_list(
        providers = [_ServiceGroupInfo],
        doc = "Services/tasks that must be started before this service/task can be started. Can be `itest_service`, `itest_task`, or `itest_service_group`.",
    ),
} | _svcinit_attrs

def _compute_env(ctx, underlying_target):
    env = {
        k: ctx.expand_location(v, targets = ctx.attr.data)
        for (k, v) in ctx.attr.env.items()
    }

    if RunEnvironmentInfo in underlying_target:
        for k, v in underlying_target[RunEnvironmentInfo].environment.items():
            if k in env:
                fail("Env key %s specified both in underlying target and itest wrapper rule" % k)
            env[k] = ctx.expand_location(v, targets = ctx.attr.data)

    return env

def _validate_deferred(ctx, deps):
    if not ctx.attr.deferred:
        for dep in deps:
            if dep[_ServiceGroupInfo].deferred:
                fail("Non-deferred itest_service cannot depend on deferred itest_service: %s depends on %s" % (ctx.label, dep.label))

def _itest_binary_impl(ctx, extra_service_spec_kwargs, extra_exe_runfiles = []):
    _validate_deferred(ctx, ctx.attr.deps)

    exe_runfiles = [ctx.attr.exe.default_runfiles] + extra_exe_runfiles

    version_file_deps = ctx.files.data + ctx.files.exe
    version_file_deps_trans = [runfiles.files for runfiles in exe_runfiles] + [data[DefaultInfo].default_runfiles.files for data in ctx.attr.data]

    version_file = _create_version_file(
        ctx,
        depset(direct = version_file_deps, transitive = version_file_deps_trans),
    )

    args = [
        ctx.expand_location(arg, targets = ctx.attr.data)
        for arg in ctx.attr.args
    ]

    if version_file:
        extra_service_spec_kwargs["version_file"] = to_rlocation_path(ctx, version_file)

    service = struct(
        label = str(ctx.label),
        exe = to_rlocation_path(ctx, ctx.executable.exe),
        args = args,
        env = _compute_env(ctx, ctx.attr.exe),
        deps = [str(dep.label) for dep in ctx.attr.deps],
        **extra_service_spec_kwargs
    )

    direct_runfiles = [version_file] if version_file else []
    transitive_runfiles = _services_runfiles(ctx, "data") + _services_runfiles(ctx, "deps") + exe_runfiles
    return _finalize_service(ctx, service, direct_runfiles, transitive_runfiles)

def _finalize_service(ctx, service, direct_runfiles = [], transitive_runfiles = []):
    """Collects transitive services, emits the svcinit spec file, and returns the providers.

    Shared by `_itest_binary_impl` (services/tasks) and `_itest_external_service_impl`.
    """
    services = _collect_services(ctx.attr.deps)
    services[service.label] = service

    service_specs_file = _create_svcinit_actions(ctx, services)

    runfiles = ctx.runfiles(ctx.files.data + [service_specs_file] + direct_runfiles)
    runfiles = runfiles.merge_all(transitive_runfiles)

    return [
        RunEnvironmentInfo(environment = _run_environment(ctx, service_specs_file)),
        DefaultInfo(runfiles = runfiles),
        _ServiceGroupInfo(services = services, deferred = ctx.attr.deferred),
    ]

def _validate_duration(name, s):
    unit = s
    for _ in range(len(s)):
        if unit[0].isdigit():
            unit = unit[1:]

    if unit not in ["ms", "s", "m", "h", "d"]:
        fail("Invalid unit for %s: %s" % (name, unit))

def _itest_service_impl(ctx):
    _validate_duration("expected_start_duration", ctx.attr.expected_start_duration)
    _validate_duration("health_check_interval", ctx.attr.health_check_interval)

    if ctx.attr.health_check_timeout:
        _validate_duration("health_check_timeout", ctx.attr.health_check_timeout)

    if ctx.attr.so_reuseport_aware and not (ctx.attr.autoassign_port or ctx.attr.named_ports or ctx.attr.ports):
        fail("SO_REUSEPORT awareness only makes sense when using port autoassignment")

    shutdown_timeout = ctx.attr.shutdown_timeout or ctx.attr._default_shutdown_timeout[BuildSettingInfo].value

    extra_service_spec_kwargs = {
        "type": "service",
        "http_health_check_address": ctx.attr.http_health_check_address,
        "port": str(ctx.attr.port[BuildSettingInfo].value),
        "autoassign_port": ctx.attr.autoassign_port,
        "so_reuseport_aware": ctx.attr.so_reuseport_aware,
        "deferred": ctx.attr.deferred,
        "domain": ctx.attr.domain,
        "port_bindings": _compute_port_bindings(ctx),
        "named_ports": {
            name: str(port_flag[BuildSettingInfo].value)
            for port_flag, name in ctx.attr.named_ports.items()
        },
        "hot_reloadable": ctx.attr.hot_reloadable,
        "expected_start_duration": ctx.attr.expected_start_duration,
        "health_check_interval": ctx.attr.health_check_interval,
        "health_check_timeout": ctx.attr.health_check_timeout,
        "shutdown_signal": ctx.attr.shutdown_signal,
        "shutdown_timeout": shutdown_timeout,
        "enforce_graceful_shutdown": bool(ctx.attr.enforce_graceful_shutdown[BuildSettingInfo].value),
    }
    extra_exe_runfiles = []

    if ctx.attr.health_check:
        extra_service_spec_kwargs["health_check_label"] = str(ctx.attr.health_check.label)
        extra_service_spec_kwargs["health_check"] = to_rlocation_path(ctx, ctx.executable.health_check)
        extra_exe_runfiles.append(ctx.attr.health_check.default_runfiles)

        health_check_args = [
            ctx.expand_location(arg, targets = ctx.attr.data)
            for arg in ctx.attr.health_check_args
        ]
        extra_service_spec_kwargs["health_check_args"] = health_check_args

    return _itest_binary_impl(ctx, extra_service_spec_kwargs, extra_exe_runfiles)

_itest_service_attrs = _itest_binary_attrs | {
    # Note, autoassigning a port is a little racy. If you can stick to hardcoded ports and network namespace, you should prefer that.
    "autoassign_port": attr.bool(
        doc = """If true, the service manager will pick a free port and assign it to the service.
        The port will be interpolated into `$${PORT}` in the service's `http_health_check_address` and `args`.
        It will also be exported under the target's fully qualified label in the service-port mapping.

        The assigned ports for all services are available for substitution in `http_health_check_address` and `args` (in case one service needs the address for another one.)
        For example, the following substitution: `args = ["-client-addr", "127.0.0.1:$${@@//label/for:service}"]`

        The service-port mapping is a JSON string -> int map propagated through the `ASSIGNED_PORTS` env var.
        For example, a port can be retrieved with the following JS code:
        `JSON.parse(process.env["ASSIGNED_PORTS"])["@@//label/for:service"]`.

        Alternately, the env will also contain the location of a binary that can return the port, for contexts without a readily-accessible JSON parser.
        For example, the following Bash command:
        `PORT=$($GET_ASSIGNED_PORT_BIN @@//label/for:service)`""",
    ),
    "port": attr.label(doc = "Internal"),
    "domain": attr.string(
        default = "127.0.0.1",
        doc = """The host that this service's ports are reachable on. Defaults to `127.0.0.1` for locally-managed services.""",
    ),
    "ports": attr.label_keyed_string_dict(
        providers = [_PortInfo],
        doc = """Maps `itest_port` targets that this service binds to a port name. The port name is used
        as the inner key in `ITEST_SERVICES_MAP`. The desired value for each port is carried by the
        `itest_port` target itself (an `int` flag, default `0` = autoassign), and can be pinned from the
        command line via `--//pkg:my_port=8080`.""",
    ),
    "named_ports": attr.label_keyed_string_dict(
        doc = """For each element of the list, the service manager will pick a free port and assign it to the service.
        The port's fully-qualified name is the service's fully-qualified label and the port name, separated by a dot.
        For example, a port assigned with `named_ports = ["http_port"]` will be assigned a fully-qualified name of `@@//label/for:service.http_port`.

        Named ports are accessible through the service-port mapping. For more details, see `autoassign_port`.""",
    ),
    "so_reuseport_aware": attr.bool(
        doc = """If set, the service manager will not release the autoassigned port. The service binary must use SO_REUSEPORT when binding it.
        This reduces the possibility of port collisions when running many service_tests in parallel, or when code binds port 0 without being
        aware of the port assignment mechanism.

        Must only be set when `autoassign_port` is enabled or `named_ports` are used.""",
    ),
    "expected_start_duration": attr.string(
        default = "0s",
        doc = "How long the service expected to take before passing a healthcheck. Any failing health checks before this duration elapses will not be logged.",
    ),
    "health_check": attr.label(
        cfg = "target",
        mandatory = False,
        executable = True,
        doc = """If set, the service manager will execute this binary to check if the service came up in a healthy state.
        This check will be retried until it exits with a 0 exit code. When used in conjunction with autoassigned ports, use
        one of the methods described in `autoassign_port` to locate the service.""",
    ),
    "health_check_args": attr.string_list(
        doc = """Arguments to pass to the health_check binary. The various defined ports will be substituted prior to being given to the health_check binary.""",
    ),
    "health_check_interval": attr.string(
        default = "200ms",
        doc = "The duration between each health check. The syntax is based on common time duration with a number, followed by the time unit. For example, `200ms`, `1s`, `2m`, `3h`, `4d`.",
    ),
    "health_check_timeout": attr.string(
        default = "",
        doc = "The timeout to wait for the health check. The syntax is based on common time duration with a number, followed by the time unit. For example, `200ms`, `1s`, `2m`, `3h`, `4d`. If empty or not set, the health check will not have a timeout.",
    ),
    "hot_reloadable": attr.bool(
        doc = """If set to True, the service manager will propagate ibazel's reload notification over stdin instead of restarting the service.
        See the ruleset docstring for more info on using ibazel""",
    ),
    "http_health_check_address": attr.string(
        doc = """If set, the service manager will send an HTTP request to this address to check if the service came up in a healthy state.
        This check will be retried until it returns a 200 HTTP code. When used in conjunction with autoassigned ports, `$${@@//label/for:service:port_name}` can be used in the address.
        Example: `http_health_check_address = "http://127.0.0.1:$${@@//label/for:service:port_name}",`""",
    ),
    "shutdown_signal": attr.string(
        default = "SIGTERM",
        doc = "The signal to send to the service when it needs to be shut down. Valid values are: SIGTERM and SIGKILL. SIGTERM is necessary to have proper coverage of services which needs to be gracefully terminated",
        values = ["SIGTERM", "SIGKILL"],
    ),
    "shutdown_timeout": attr.string(
        doc = "The duration to wait by default after sending the shutdown signal before forcefully killing the service. The syntax is based on common time duration with a number, followed by the time unit. For example, `200ms`, `1s`, `2m`, `3h`, `4d`. If not defined, the value of `_default_shutdown_timeout` will be used.",
    ),
    "_default_shutdown_timeout": attr.label(
        default = "//:shutdown_timeout",
        doc = "The duration to wait by default after sending the shutdown signal before forcefully killing the service. The syntax is based on common time duration with a number, followed by the time unit. For example, `200ms`, `1s`, `2m`, `3h`, `4d`.",
    ),
    "enforce_graceful_shutdown": attr.label(
        default = "//:enforce_graceful_shutdown",
        doc = """If set to True, the service manager will fail the service_test if the service had to be forcefully killed if the signal was not SIGKILL and after the shutdown timeout elapsed.

        This needs to be False to have coverage of your services but don't want a them to be graceful at shutdown""",
    ),
}

itest_service = rule(
    implementation = _itest_service_impl,
    attrs = _itest_service_attrs,
    executable = True,
    doc = """An itest_service is a binary that is intended to run for the duration of the integration test. Examples include databases, HTTP/RPC servers, queue consumers, external service mocks, etc.

All [common binary attributes](https://bazel.build/reference/be/common-definitions#common-attributes-binaries) are supported including `args`.""",
)

def _external_port_value(ctx, name):
    if name not in ctx.attr.port_numbers:
        fail("itest_external_service %s: no port number provided in `port_numbers` for port %r" % (ctx.label, name))
    return ctx.attr.port_numbers[name]

def _itest_external_service_impl(ctx):
    _validate_deferred(ctx, ctx.attr.deps)

    if ctx.attr.health_check_interval:
        _validate_duration("health_check_interval", ctx.attr.health_check_interval)
    if ctx.attr.health_check_timeout:
        _validate_duration("health_check_timeout", ctx.attr.health_check_timeout)

    bindings = _bind_port_targets(ctx, lambda _port_target, name: _external_port_value(ctx, name))

    extra_service_spec_kwargs = {
        "type": "external_service",
        "domain": ctx.attr.domain,
        "port_bindings": bindings,
        "http_health_check_address": ctx.attr.http_health_check_address,
        "expected_start_duration": ctx.attr.expected_start_duration,
        "health_check_interval": ctx.attr.health_check_interval,
        "health_check_timeout": ctx.attr.health_check_timeout,
        "deferred": ctx.attr.deferred,
    }
    extra_exe_runfiles = []

    if ctx.attr.health_check:
        extra_service_spec_kwargs["health_check_label"] = str(ctx.attr.health_check.label)
        extra_service_spec_kwargs["health_check"] = to_rlocation_path(ctx, ctx.executable.health_check)
        extra_exe_runfiles.append(ctx.attr.health_check.default_runfiles)
        extra_service_spec_kwargs["health_check_args"] = [
            ctx.expand_location(arg, targets = ctx.attr.data)
            for arg in ctx.attr.health_check_args
        ]

    service = struct(
        label = str(ctx.label),
        exe = "",
        args = [],
        env = {},
        deps = [str(dep.label) for dep in ctx.attr.deps],
        **extra_service_spec_kwargs
    )

    return _finalize_service(ctx, service, transitive_runfiles = _services_runfiles(ctx, "deps") + extra_exe_runfiles)

_itest_external_service_attrs = {
    "domain": attr.string(
        mandatory = True,
        doc = "The fully-qualified domain name (FQDN) that this external service is reachable on, e.g. `my_service.test.mycompany.com`.",
    ),
    "ports": attr.label_keyed_string_dict(
        providers = [_PortInfo],
        doc = "Maps `itest_port` targets that this external service exposes to a port name. Provide the literal port number for each name via `port_numbers`.",
    ),
    "port_numbers": attr.string_dict(
        doc = "Maps each port name (from `ports`) to the literal port number it is reachable on at the FQDN.",
    ),
    "data": attr.label_list(allow_files = True),
    "deps": attr.label_list(
        providers = [_ServiceGroupInfo],
        doc = "Services/tasks that must be available before this external service can be used.",
    ),
    "deferred": attr.bool(
        doc = "If set, the external service will not be health-checked on boot up.",
    ),
    "http_health_check_address": attr.string(
        doc = "If set, the service manager will send an HTTP request to this address to verify the external service is reachable. Port substitutions (including `$${<port>::origin}`) are supported.",
    ),
    "health_check": attr.label(
        cfg = "target",
        mandatory = False,
        executable = True,
        doc = "If set, the service manager will execute this binary to verify the external service is reachable.",
    ),
    "health_check_args": attr.string_list(
        doc = "Arguments to pass to the health_check binary. Port substitutions are applied prior to execution.",
    ),
    "health_check_interval": attr.string(
        default = "200ms",
        doc = "The duration between each health check.",
    ),
    "health_check_timeout": attr.string(
        default = "",
        doc = "The timeout to wait for the health check. If empty, the health check will not have a timeout.",
    ),
    "expected_start_duration": attr.string(
        default = "0s",
        doc = "How long the service is expected to take before passing a health check. Failing checks before this elapses are not logged.",
    ),
} | _svcinit_attrs

itest_external_service = rule(
    implementation = _itest_external_service_impl,
    attrs = _itest_external_service_attrs,
    executable = True,
    doc = """An itest_external_service points at a production or production-like instance of a service that is
reachable at a fixed FQDN, rather than a binary that the service manager spawns locally.

The service manager never starts or stops external services. If a health check is configured, it will be run
to verify the external service is reachable before dependent services/tests run.

Because it provides the same information as an `itest_service`, it can be swapped in for a local service using
Bazel `select()` to run a test suite against production-like instances.""",
)

def _itest_port_impl(ctx):
    return [
        _PortInfo(
            label = str(ctx.label),
            aliases = ctx.attr.aliases,
        ),
        BuildSettingInfo(value = ctx.build_setting_value),
    ]

itest_port = rule(
    implementation = _itest_port_impl,
    build_setting = config.int(flag = True),
    attrs = {
        "aliases": attr.string_list(
            doc = """Additional keys that must resolve to this port in `ITEST_PORTS_MAP` and the `ASSIGNED_PORTS`
            map. Useful for keeping backwards-compatible references working. The port target's own label is
            always bound, in addition to any aliases.""",
        ),
    },
    doc = """Declares a port as a first-class target. A port is a pure handle: the host is supplied by the
internal or external service that binds it, while the desired value is carried by the port target itself as
an `int` build-setting flag (`0` = autoassign for internal services). A given port may only be bound by a
single service.

Because the port is a flag, its value can be pinned from the command line, e.g. `--//pkg:my_port=8080`.

Ports are always referenced by their target label (e.g. via `port` / `port_ref`), and are bound exactly once.""",
)

def _itest_task_impl(ctx):
    return _itest_binary_impl(ctx, {
        "type": "task",
    })

itest_task = rule(
    implementation = _itest_task_impl,
    attrs = _itest_binary_attrs,
    executable = True,
    doc = """A task is a one-shot execution of a binary that is intended to run as part of the itest scenario creation.
Examples include: filesystem setup, dynamic config file generation (especially if it depends on ports), DB migrations or seed data creation.

All [common binary attributes](https://bazel.build/reference/be/common-definitions#common-attributes-binaries) are supported including `args`.""",
)

def _itest_service_group_impl(ctx):
    _validate_deferred(ctx, ctx.attr.services)

    services = _collect_services(ctx.attr.services)

    service = struct(
        type = "group",
        label = str(ctx.label),
        deps = [str(service.label) for service in ctx.attr.services],
        port_aliases = ctx.attr.port_aliases,
    )
    services[service.label] = service

    service_specs_file = _create_svcinit_actions(ctx, services)

    runfiles = ctx.runfiles([service_specs_file])
    runfiles = runfiles.merge_all(_services_runfiles(ctx))

    return [
        RunEnvironmentInfo(environment = _run_environment(ctx, service_specs_file)),
        DefaultInfo(runfiles = runfiles),
        _ServiceGroupInfo(services = services, deferred = ctx.attr.deferred),
    ]

_itest_service_group_attrs = _svcinit_attrs | {
    "deferred": attr.bool(
        doc = """If set, the group will not be started on boot up. It can be started using the service manager's control API.""",
    ),
    "port_aliases": attr.string_dict(
        doc = """Port aliases allow you to 're-export' another service's port as belonging to this service group.
This can be used to create abstractions (such as an itest_service combined with an itest_task) but not leak
their implementation naming details through how client code accesses port names.

Use the functions `port_alias` and `named_port_alias` to reference ports from the services to alias ports for""",
    ),
    "services": attr.label_list(
        providers = [_ServiceGroupInfo],
        doc = "Services/tasks that comprise this group. Can be `itest_service`, `itest_task`, or `itest_service_group`.",
    ),
}

itest_service_group = rule(
    implementation = _itest_service_group_impl,
    attrs = _itest_service_group_attrs,
    executable = True,
    doc = """A service group is a collection of services/tasks.

It serves as a convenient way for a downstream target to depend on multiple services with a single label, without
forcing the services within the group to define a specific startup ordering with their `deps`.

It can bring up multiple services with a single `bazel run` command, which is useful for creating dev environments.""",
)

def _create_svcinit_actions(ctx, services):
    _validate_unique_port_bindings(services)

    ctx.actions.symlink(
        output = ctx.outputs.executable,
        target_file = ctx.executable._svcinit,
    )

    # Avoid expanding during analysis phase.
    service_content = ctx.actions.args()
    service_content.set_param_file_format("multiline")
    service_content.add_all([services], map_each = json.encode)

    service_specs_file = ctx.actions.declare_file(ctx.label.name + ".service_specs.json")
    ctx.actions.write(
        output = service_specs_file,
        content = service_content,
    )

    return service_specs_file

def _service_test_impl(ctx):
    service_specs_file = _create_svcinit_actions(
        ctx,
        _collect_services(ctx.attr.services),
    )

    env_file = ctx.actions.declare_file(ctx.label.name + ".env.json")
    ctx.actions.write(
        output = env_file,
        content = json.encode(_compute_env(ctx, ctx.attr.test)),
    )

    fixed_env = _run_environment(ctx, service_specs_file)
    fixed_env["SVCINIT_TEST_RLOCATION_PATH"] = to_rlocation_path(ctx, ctx.executable.test)
    fixed_env["SVCINIT_TEST_ENV_RLOCATION_PATH"] = to_rlocation_path(ctx, env_file)

    runfiles = ctx.runfiles(ctx.files.data + [service_specs_file, env_file])
    runfiles = runfiles.merge_all(_services_runfiles(ctx) + [
        ctx.attr.test.default_runfiles,
    ])

    return [
        RunEnvironmentInfo(environment = fixed_env),
        DefaultInfo(runfiles = runfiles),
    ]

_service_test_attrs = {
    "test": attr.label(
        cfg = "target",
        executable = True,
        doc = "The underlying test target to execute once the services have been brought up and healthchecked.",
    ),
    "env": attr.string_dict(
        doc = "The service manager will merge these variables into the environment when spawning the underlying binary.",
    ),
    "data": attr.label_list(allow_files = True),
    ## This is taken directly from rules_go: https://github.com/bazel-contrib/rules_go/blob/85eef05357c9421eaa568d101e62355384bc49bb/go/private/rules/test.bzl#L442-L457
    # Required for Bazel to merge coverage reports for Go and other
    # languages into a single report per test.
    # Using configuration_field ensures that the tool is only built when
    # run with bazel coverage, not with bazel test.
    "_lcov_merger": attr.label(
        default = configuration_field(fragment = "coverage", name = "output_generator"),
        cfg = "exec",
    ),
} | _itest_service_group_attrs

service_test = rule(
    implementation = _service_test_impl,
    attrs = _service_test_attrs,
    test = True,
    doc = """Brings up a set of services/tasks and runs a test target against them.

This can be used to customize which services a particular test needs while being able to bring them up in an easy and consistent way.

Example usage:
```
go_test(
    name = "_example_test_no_services",
    srcs = [..],
    tags = ["manual"],
)

service_test(
    name = "example_test",
    test = ":_example_test_no_services",
    services = [
        "//services/mysql",
        ...
    ],
)
```

Typically this would be wrapped into a macro.

All [common binary attributes](https://bazel.build/reference/be/common-definitions#common-attributes-binaries) are supported including `args`.""",
)

def _create_version_file(ctx, inputs):
    if not ctx.attr._enable_per_service_reload[BuildSettingInfo].value:
        return None

    output = ctx.actions.declare_file(ctx.label.name + ".version")

    ctx.actions.run_shell(
        inputs = inputs,
        tools = [],  # Ensure inputs in the host configuration are not treated specially.
        outputs = [output],
        command = "/bin/date > {}".format(
            output.path,
        ),
        mnemonic = "SvcVersionFile",
        # disable remote cache and sandbox, since the output is not stable given the inputs
        # additionally, running this action in the sandbox is way too expensive
        execution_requirements = {"local": "1", "no-cache": "1"},
    )

    return output
