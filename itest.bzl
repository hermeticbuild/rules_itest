""" Rules for running services in integration tests. """

load("@bazel_skylib//rules:common_settings.bzl", "int_flag")
load(
    "//private:itest.bzl",
    _itest_external_service = "itest_external_service",
    _itest_port = "itest_port",
    _itest_service = "itest_service",
    _itest_service_group = "itest_service_group",
    _itest_task = "itest_task",
    _service_test = "service_test",
)

def _to_relative_port(label):
    return str(native.package_relative_label(label))

def _to_relative_named_port(label, name):
    return _to_relative_port(label + "." + name)

def port(label):
    """This function is used to reference the auto assigned port of a service in the `args` or `env` attributes of an `itest_service` or `itest_task`.

    To reference the auto assigned port in the `itest_service_group` use `port_alias` instead
    """
    return "$${%s}" % _to_relative_port(label)

def named_port(label, name):
    """This function is used to reference a named port of a service in the `args` or `env` attributes of an `itest_service` or `itest_task`.

    To reference a named port in the `itest_service_group` use `named_port_alias` instead
    """
    return port(_to_relative_named_port(label, name))

def port_alias(label):
    """This function is used to reference the auto assigned port of a service in the `aliases` attribute of an `itest_service_group`.

    To reference the auto assigned port in `itest_service` or `itest_task` use `port` instead
    """
    return _to_relative_port(label)

def named_port_alias(label, name):
    """This function is used to reference a named port of a service in the `aliases` attribute of an `itest_service_group`.

    To reference a named port in `itest_service` or `itest_task` use `named_port` instead
    """
    return _to_relative_named_port(label, name)

def port_ref(label):
    """References the assigned port of an `itest_port` target in the `args` or `env` of an `itest_service`/`itest_task`.

    This is equivalent to `port`, but is named to make it clear it points at an `itest_port` target.
    """
    return "$${%s}" % _to_relative_port(label)

def port_origin(label):
    """References the origin (`<domain>:<port>`) of an `itest_port` target in `args`/`env`/`http_health_check_address`."""
    return "$${%s::origin}" % _to_relative_port(label)

def port_domain(label):
    """References the domain (host) of an `itest_port` target in `args`/`env`/`http_health_check_address`."""
    return "$${%s::domain}" % _to_relative_port(label)

def itest_port(name, build_setting_default = 0, **kwargs):
    """Declares a first-class port target.

    The port is an `int` build-setting flag; `0` (the default) means autoassign for the internal service
    that binds it. The value can be pinned from the command line via `--//pkg:name=8080`.
    """
    _itest_port(
        name = name,
        build_setting_default = build_setting_default,
        **kwargs
    )

def itest_service(name, tags = [], hygienic = True, named_ports = [], **kwargs):
    if "port" in kwargs:
        fail("Do not specify `port`, instead set it via the `%s` flag" % (name + ".port"))

    int_flag(
        name = name + ".port",
        build_setting_default = 0,
    )

    named_ports_attr = {}
    for named_port in named_ports:
        named_port_label = name + "." + named_port
        named_ports_attr[named_port_label] = named_port

        int_flag(
            name = named_port_label,
            build_setting_default = 0,
        )

    _itest_service(
        name = name,
        tags = tags + ["ibazel_notify_changes"],
        port = name + ".port",
        named_ports = named_ports_attr,
        **kwargs
    )

    if hygienic:
        _hygiene_test(
            name = name,
            tags = tags,
        )

def itest_external_service(name, tags = [], hygienic = True, **kwargs):
    _itest_external_service(
        name = name,
        tags = tags + ["ibazel_notify_changes"],
        **kwargs
    )

    if hygienic:
        _hygiene_test(
            name = name,
            tags = tags,
        )

def itest_service_group(name, tags = [], hygienic = True, **kwargs):
    _itest_service_group(
        name = name,
        tags = tags + ["ibazel_notify_changes"],
        **kwargs
    )

    if hygienic:
        _hygiene_test(
            name = name,
            tags = tags,
        )

def itest_task(name, tags = [], hygienic = True, **kwargs):
    _itest_task(
        name = name,
        tags = tags + ["ibazel_notify_changes"],
        **kwargs
    )

    if hygienic:
        _hygiene_test(
            name = name,
            tags = tags,
        )

def _hygiene_test(name, **kwargs):
    service_test(
        name = name + "_hygiene_test",
        services = [name],
        test = "@rules_itest//:exit0",
        **kwargs
    )

service_test = _service_test

# The underlying rules are exported so that they can be more easily extended by users
# (for example, to wrap them in their own macros). Prefer the macros above for normal usage.
itest_service_rule = _itest_service
itest_task_rule = _itest_task
itest_service_group_rule = _itest_service_group
itest_external_service_rule = _itest_external_service
