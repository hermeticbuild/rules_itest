def _shell_quote(value):
    return "'" + value.replace("'", "'\"'\"'") + "'"

def _cleanup_failure_test_impl(ctx):
    test_env = ctx.attr.test[RunEnvironmentInfo].environment
    test_path = ctx.executable.test.short_path

    env_lines = [
        "export {}={}".format(key, _shell_quote(value))
        for key, value in sorted(test_env.items())
    ]

    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = script,
        is_executable = True,
        content = """#!/usr/bin/env bash
set -euo pipefail

cleanup_marker="${{TEST_TMPDIR}}/startup_failure_cleanup_marker"
log_file="${{TEST_TMPDIR}}/dependent_startup_failure.log"
test_path="${{TEST_SRCDIR}}/${{TEST_WORKSPACE}}/{test_path}"

rm -f "${{cleanup_marker}}"

{env}

set +e
"${{test_path}}" >"${{log_file}}" 2>&1
status=$?
set -e

cat "${{log_file}}"

if [[ "${{status}}" -eq 0 ]]; then
    echo "expected dependent startup failure test to fail" >&2
    exit 1
fi

if [[ ! -f "${{cleanup_marker}}" ]]; then
    echo "expected already-started service to receive shutdown before svcinit exited" >&2
    exit 1
fi

echo "cleanup marker found: $(cat "${{cleanup_marker}}")"
""".format(
            env = "\n".join(env_lines),
            test_path = test_path,
        ),
    )

    runfiles = ctx.runfiles(files = [ctx.executable.test])
    runfiles = runfiles.merge(ctx.attr.test.default_runfiles)

    return [
        DefaultInfo(
            executable = script,
            runfiles = runfiles,
        ),
    ]

cleanup_failure_test = rule(
    implementation = _cleanup_failure_test_impl,
    attrs = {
        "test": attr.label(
            executable = True,
            cfg = "target",
            providers = [RunEnvironmentInfo],
        ),
    },
    test = True,
)
