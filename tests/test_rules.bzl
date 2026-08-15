"""Integration-test rules for the test module's supported languages."""

load("@aspect_rules_js//js/private:js_binary.bzl", _js_test = "js_test")
load("@bazel_skylib//rules:native_binary.bzl", _native_test = "native_test")
load("@rules_go//go:def.bzl", _go_test = "go_test")
load("@rules_itest//:itest.bzl", "extend_test_rule")
load("@rules_shell//shell/private:sh_test.bzl", _sh_test = "sh_test")

itest_go_test = extend_test_rule(_go_test)
itest_js_test = extend_test_rule(_js_test)
itest_native_test = extend_test_rule(_native_test)
itest_sh_test = extend_test_rule(_sh_test)
