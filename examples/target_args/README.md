# Delegating `bazel run` arguments to itest targets

This example shows how `bazel run` can append extra arguments and inject extra environment variables into specific `itest_*` targets in the graph, including Bazel aliases.

Run it from the `examples/` workspace:

```text
bazel run //target_args:workflow -- \
  --target_env //target_args:migrate_alias TARGET_ARGS_INJECTED_ENV=migrate-env \
  --target_arg //target_args:migrate_alias --migration=users --dry-run \
  --target_env //target_args:seed_alias "TARGET_ARGS_INJECTED_ENV=seed env" \
  --target_arg //target_args:seed_alias "--users=alice bob"
```

Expected output includes the `TARGET_ARGS_` environment variables for each task, followed by its delegated args. For example:

```text
@@//target_args:migrate> TARGET_ARGS_INJECTED_ENV=migrate-env
@@//target_args:migrate> TARGET_ARGS_TASK_NAME=//target_args:migrate
@@//target_args:migrate> ARGS=["--base-flag=migrate-default" "--migration=users" "--dry-run"]
@@//target_args:seed> TARGET_ARGS_INJECTED_ENV=seed env
@@//target_args:seed> TARGET_ARGS_TASK_NAME=//target_args:seed
@@//target_args:seed> ARGS=["--base-flag=seed-default" "--users=alice bob"]
```

`bazel run` mode keeps the service manager alive after the tasks finish, so press `Ctrl-C` once the output is printed.
