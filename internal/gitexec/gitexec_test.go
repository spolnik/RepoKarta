package gitexec

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestRunKeepsStderrOutOfStdout(t *testing.T) {
	result, err := Run(context.Background(), Options{}, "not-a-real-subcommand")
	if err == nil {
		t.Fatal("invalid Git command unexpectedly succeeded")
	}
	if len(result.Stdout) != 0 {
		t.Fatalf("stdout = %q, want empty", result.Stdout)
	}
	if len(result.Stderr) == 0 {
		t.Fatal("stderr was not captured")
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("error = %T, want wrapped *exec.ExitError", err)
	}
}

func TestRunHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Options{}, "--version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestEnvironmentReplacesOwnedVariables(t *testing.T) {
	values := environment([]string{
		"PATH=test",
		"git_optional_locks=1",
		"GIT_EXTERNAL_DIFF=unsafe",
	})
	for _, expected := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_EXTERNAL_DIFF=",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"PATH=test",
	} {
		if !slices.Contains(values, expected) {
			t.Errorf("environment is missing %q: %#v", expected, values)
		}
	}
	for _, value := range values {
		if strings.EqualFold(value, "git_optional_locks=1") ||
			strings.EqualFold(value, "GIT_EXTERNAL_DIFF=unsafe") {
			t.Errorf("unsafe inherited value remained: %q", value)
		}
	}
}

func TestRepositoryArgumentsPreferBareDirectory(t *testing.T) {
	got := repositoryArguments(
		Repository{Directory: "work", GitDirectory: "bare.git"},
		[]string{"status"},
	)
	want := []string{"--git-dir", "bare.git", "status"}
	if !slices.Equal(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}
