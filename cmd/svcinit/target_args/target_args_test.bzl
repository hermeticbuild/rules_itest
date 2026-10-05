def _shell_quote(value):
    return "'" + value.replace("'", "'\"'\"'") + "'"

def _target_args_test_impl(ctx):
    target_env = ctx.attr.target_under_test[RunEnvironmentInfo].environment
    target_path = ctx.executable.target_under_test.short_path

    env_lines = [
        "export {}={}".format(key, _shell_quote(value))
        for key, value in sorted(target_env.items())
    ]
    cleanup_lines = [
        "rm -f \"${{TEST_TMPDIR}}/{}\"".format(path)
        for path in sorted(ctx.attr.expected_files.keys())
    ]
    expected_paths = [
        "\"${{TEST_TMPDIR}}/{}\"".format(path)
        for path in sorted(ctx.attr.expected_files.keys())
    ]
    check_blocks = []
    for path, content in sorted(ctx.attr.expected_files.items()):
        check_blocks.append("""expected_path="${{TEST_TMPDIR}}/{path}.expected"
actual_path="${{TEST_TMPDIR}}/{path}"
cat <<'EOF' > "${{expected_path}}"
{content}
EOF
if [[ ! -f "${{actual_path}}" ]]; then
    echo "missing output file: ${{actual_path}}" >&2
    exit 1
fi
diff -u "${{expected_path}}" "${{actual_path}}"
""".format(path = path, content = content))

    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = script,
        is_executable = True,
        content = """#!/usr/bin/env bash
set -euo pipefail

target_path="${{TEST_SRCDIR}}/${{TEST_WORKSPACE}}/{target_path}"

{cleanup}
{env}
unset TEST_TARGET TEST_SIZE TEST_TIMEOUT XML_OUTPUT_FILE

"${{target_path}}" {argv} >"${{TEST_TMPDIR}}/target.log" 2>&1 &
target_pid=$!
trap 'kill "${{target_pid}}" 2>/dev/null || true; wait "${{target_pid}}" 2>/dev/null || true' EXIT

for _ in $(seq 1 100); do
    missing=0
    for expected_file in {expected_paths}; do
        if [[ ! -f "${{expected_file}}" ]]; then
            missing=1
            break
        fi
    done
    if [[ "${{missing}}" -eq 0 ]]; then
        break
    fi
    if ! kill -0 "${{target_pid}}" 2>/dev/null; then
        cat "${{TEST_TMPDIR}}/target.log"
        echo "target exited before producing expected files" >&2
        exit 1
    fi
    sleep 0.1
done

if [[ "${{missing}}" -ne 0 ]]; then
    cat "${{TEST_TMPDIR}}/target.log"
    echo "timed out waiting for expected files" >&2
    exit 1
fi

kill "${{target_pid}}"
wait "${{target_pid}}" || true
trap - EXIT

{checks}
""".format(
            target_path = target_path,
            cleanup = "\n".join(cleanup_lines),
            env = "\n".join(env_lines),
            expected_paths = " ".join(expected_paths),
            argv = " ".join([_shell_quote(arg) for arg in ctx.attr.argv]),
            checks = "\n".join(check_blocks),
        ),
    )

    runfiles = ctx.runfiles(files = [ctx.executable.target_under_test])
    runfiles = runfiles.merge(ctx.attr.target_under_test.default_runfiles)

    return [
        DefaultInfo(
            executable = script,
            runfiles = runfiles,
        ),
    ]

target_args_test = rule(
    implementation = _target_args_test_impl,
    attrs = {
        "argv": attr.string_list(),
        "expected_files": attr.string_dict(),
        "target_under_test": attr.label(
            executable = True,
            cfg = "target",
            providers = [RunEnvironmentInfo],
        ),
    },
    test = True,
)
