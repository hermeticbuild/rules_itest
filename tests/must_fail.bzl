load("@bazel_lib//lib:paths.bzl", "to_rlocation_path")

def _must_fail_test_impl(ctx):
    if not ctx.attr.expected:
        fail("expected failure message cannot be empty", attr = "expected")
    executable = ctx.actions.declare_file(ctx.label.name + ".exe")
    ctx.actions.symlink(
        output = executable,
        target_file = ctx.executable._wrapper,
        is_executable = True,
    )

    # service_test outputs have no extension. os/exec needs an .exe on Windows.
    subject = ctx.actions.declare_file(ctx.label.name + ".subject.exe")
    ctx.actions.symlink(output = subject, target_file = ctx.executable.test, is_executable = True)
    runfiles = ctx.runfiles(files = [subject]).merge_all([
        ctx.attr.test[DefaultInfo].default_runfiles,
        ctx.attr._wrapper[DefaultInfo].default_runfiles,
    ])
    env = ctx.attr.test[RunEnvironmentInfo]
    return [
        DefaultInfo(executable = executable, runfiles = runfiles),
        RunEnvironmentInfo(
            environment = env.environment | {
                "EXPECTED_FAILURE": ctx.attr.expected,
                "TEST_TO_RUN": to_rlocation_path(ctx, subject),
            },
            inherited_environment = env.inherited_environment,
        ),
    ]

must_fail_test = rule(
    implementation = _must_fail_test_impl,
    attrs = {
        "expected": attr.string(mandatory = True),
        "test": attr.label(
            mandatory = True,
            executable = True,
            cfg = "target",
            providers = [RunEnvironmentInfo],
        ),
        "_wrapper": attr.label(default = "//:must_fail", executable = True, cfg = "target"),
    },
    test = True,
)
