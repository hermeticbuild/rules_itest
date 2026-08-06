load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")

def _double_bind_test(ctx):
    """Verifies that binding the same port from two services fails analysis."""
    env = analysistest.begin(ctx)

    asserts.expect_failure(
        env,
        "Port @@//ports:shared_port is bound by multiple services: @@//ports:binder_a and @@//ports:binder_b. A port may only be bound once.",
    )

    return analysistest.end(env)

double_bind_test = analysistest.make(
    _double_bind_test,
    expect_failure = True,
)

def tests():
    double_bind_test(
        name = "test_double_bind_should_fail",
        target_under_test = ":double_bind",
    )
