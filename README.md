# Bazel rules for integration testing and dev environments

This repository provides a Bazel ruleset for running services in a hermetic way.
It can be used to provision development environments or to run tests against services.

For example, you may want to run integration tests that depend on services like a database, Redis, etc., or you may want to integration-test first-party binaries.

With this ruleset, each test can declare the services it depends on. The test will then be provisioned with a fresh instance of those services, each verified to have passed health checks before your test code runs.

This ruleset also integrates with [ibazel](https://github.com/bazelbuild/bazel-watcher) to support incremental hot-reload during development.

# Usage
See [docs/itest.md](https://github.com/hermeticbuild/rules_itest/blob/master/docs/itest.md) for full documentation.

Per-service reload under ibazel works by injecting a cache-busting input, so it is disabled by default to keep tests cacheable. You can enable it setting the `--@rules_itest//:enable_per_service_reload` flag.

## Note
`ibazel` invokes `cquery` which does not accept Starlark flags. Thus you will want to configure the flag in `.bazelrc` and pass it via `--config`.
A common setup looks like this:
```
.bazelrc

common:enable-reload --@rules_itest//:enable_per_service_reload
```

`ibazel run --config enable-reload //path/to:target`

# Examples
First-party service examples (Go and Node.js binaries): [tests folder](https://github.com/hermeticbuild/rules_itest/tree/master/tests).

More third-party examples (MySQL, Redis, DynamoDB): [examples folder](https://github.com/hermeticbuild/rules_itest/tree/master/examples).
