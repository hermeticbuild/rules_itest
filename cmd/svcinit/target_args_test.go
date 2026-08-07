package main

import (
	"reflect"
	"strings"
	"testing"

	"rules_itest/svclib"
)

func TestParseDelegatedTargetArgs(t *testing.T) {
	serviceSpecs := map[string]svclib.ServiceSpec{
		"@@//cmd/svcinit/target_args:dep_task": {Type: "task"},
		"@@//cmd/svcinit/target_args:top_task": {Type: "task"},
	}
	aliases := map[string][]string{
		"@@//cmd/svcinit/target_args:dep_alias": {"@@//cmd/svcinit/target_args:dep_task"},
	}

	got, err := parseDelegatedTargetArgs([]string{
		"--target_arg", "top_task",
		"--top-flag",
		"--top-pair", "with value",
		"--target_arg", "dep_alias",
		"--dep-flag",
	}, serviceSpecs, aliases)
	if err != nil {
		t.Fatalf("parseDelegatedTargetArgs() error = %v", err)
	}

	want := map[string][]string{
		"@@//cmd/svcinit/target_args:top_task": {"--top-flag", "--top-pair", "with value"},
		"@@//cmd/svcinit/target_args:dep_task": {"--dep-flag"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDelegatedTargetArgs() mismatch (-want +got):\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestParseDelegatedTargetConfigIncludesEnv(t *testing.T) {
	serviceSpecs := map[string]svclib.ServiceSpec{
		"@@//cmd/svcinit/target_args:dep_task": {Type: "task"},
		"@@//cmd/svcinit/target_args:top_task": {Type: "task"},
	}
	aliases := map[string][]string{
		"@@//cmd/svcinit/target_args:dep_alias": {"@@//cmd/svcinit/target_args:dep_task"},
	}

	got, err := parseDelegatedTargetConfig([]string{
		"--target_arg", "//cmd/svcinit/target_args:top_task",
		"--top-flag",
		"--target_env", "//cmd/svcinit/target_args:top_task", "TOP_ENV=hello world",
		"--target_env", "dep_alias", "DEP_ENV=1",
	}, serviceSpecs, aliases)
	if err != nil {
		t.Fatalf("parseDelegatedTargetConfig() error = %v", err)
	}

	want := delegatedTargetConfig{
		Args: map[string][]string{
			"@@//cmd/svcinit/target_args:top_task": {"--top-flag"},
		},
		Env: map[string]map[string]string{
			"@@//cmd/svcinit/target_args:top_task": {"TOP_ENV": "hello world"},
			"@@//cmd/svcinit/target_args:dep_task": {"DEP_ENV": "1"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDelegatedTargetConfig() mismatch (-want +got):\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestParseDelegatedEnvAssignment(t *testing.T) {
	key, value, err := parseDelegatedEnvAssignment("FOO=")
	if err != nil {
		t.Fatalf("parseDelegatedEnvAssignment() error = %v", err)
	}
	if key != "FOO" || value != "" {
		t.Fatalf("parseDelegatedEnvAssignment() = (%q, %q), want (%q, %q)", key, value, "FOO", "")
	}
}

func TestResolveDelegatedTargetRejectsGroupAlias(t *testing.T) {
	serviceSpecs := map[string]svclib.ServiceSpec{
		"@@//cmd/svcinit/target_args:group": {Type: "group"},
	}
	aliases := map[string][]string{
		"@@//cmd/svcinit/target_args:group_alias": {"@@//cmd/svcinit/target_args:group"},
	}

	_, err := resolveDelegatedTarget("group_alias", serviceSpecs, aliases)
	if err == nil {
		t.Fatal("resolveDelegatedTarget() error = nil, want group rejection")
	}
	if !strings.Contains(err.Error(), "non-executable itest_service_group") {
		t.Fatalf("resolveDelegatedTarget() error = %q, want group rejection", err)
	}
}
