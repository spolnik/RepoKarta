// Package gitexec owns bounded, non-interactive Git process execution.
package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const DefaultTimeout = 20 * time.Second

// Repository selects either a working tree or a bare Git directory. Leave
// both fields empty for commands such as git --version.
type Repository struct {
	Directory    string
	GitDirectory string
}

// Options controls the command deadline and repository location.
type Options struct {
	Repository Repository
	Timeout    time.Duration
}

// Result keeps stdout and stderr separate so warnings cannot contaminate
// parsed command output.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// Error describes a failed bounded Git invocation and preserves the original
// process error for errors.Is/errors.As.
type Error struct {
	Arguments []string
	Stderr    string
	TimedOut  bool
	Err       error
}

func (e *Error) Error() string {
	command := strings.Join(e.Arguments, " ")
	if e.TimedOut {
		return fmt.Sprintf("git %s timed out", command)
	}
	if e.Stderr != "" {
		return fmt.Sprintf("git %s: %v: %s", command, e.Err, e.Stderr)
	}
	return fmt.Sprintf("git %s: %v", command, e.Err)
}

func (e *Error) Unwrap() error {
	return e.Err
}

// Invocation exposes a configured command for streaming use cases such as
// git cat-file --batch. Close releases its deadline resources.
type Invocation struct {
	Command   *exec.Cmd
	Context   context.Context
	Arguments []string
	cancel    context.CancelFunc
}

// New creates a bounded Git invocation with a sanitized, non-interactive
// environment.
func New(ctx context.Context, options Options, arguments ...string) *Invocation {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	commandArguments := repositoryArguments(options.Repository, arguments)
	command := exec.CommandContext(bounded, "git", commandArguments...)
	command.Env = environment(os.Environ())
	return &Invocation{
		Command:   command,
		Context:   bounded,
		Arguments: append([]string(nil), arguments...),
		cancel:    cancel,
	}
}

// Close releases resources associated with the command deadline.
func (i *Invocation) Close() {
	if i != nil && i.cancel != nil {
		i.cancel()
	}
}

// Run executes one Git command and returns separated output.
func Run(ctx context.Context, options Options, arguments ...string) (Result, error) {
	invocation := New(ctx, options, arguments...)
	defer invocation.Close()
	var stdout, stderr bytes.Buffer
	invocation.Command.Stdout = &stdout
	invocation.Command.Stderr = &stderr
	err := invocation.Command.Run()
	result := Result{
		Stdout: append([]byte(nil), stdout.Bytes()...),
		Stderr: append([]byte(nil), stderr.Bytes()...),
	}
	if err == nil {
		return result, nil
	}
	if invocation.Context.Err() != nil {
		err = invocation.Context.Err()
	}
	return result, &Error{
		Arguments: append([]string(nil), arguments...),
		Stderr:    strings.TrimSpace(stderr.String()),
		TimedOut:  errors.Is(invocation.Context.Err(), context.DeadlineExceeded),
		Err:       err,
	}
}

func repositoryArguments(repository Repository, arguments []string) []string {
	capacity := len(arguments)
	if repository.Directory != "" || repository.GitDirectory != "" {
		capacity += 2
	}
	output := make([]string, 0, capacity)
	if repository.GitDirectory != "" {
		output = append(output, "--git-dir", repository.GitDirectory)
	} else if repository.Directory != "" {
		output = append(output, "-C", repository.Directory)
	}
	return append(output, arguments...)
}

func environment(current []string) []string {
	owned := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_EXTERNAL_DIFF":   "",
		"GIT_OPTIONAL_LOCKS":  "0",
		"GIT_TERMINAL_PROMPT": "0",
		"LC_ALL":              "C",
	}
	output := make([]string, 0, len(current)+len(owned))
	for _, entry := range current {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := owned[strings.ToUpper(name)]; replaced {
				continue
			}
		}
		output = append(output, entry)
	}
	for _, name := range []string{
		"GIT_CONFIG_NOSYSTEM",
		"GIT_EXTERNAL_DIFF",
		"GIT_OPTIONAL_LOCKS",
		"GIT_TERMINAL_PROMPT",
		"LC_ALL",
	} {
		output = append(output, name+"="+owned[name])
	}
	return output
}
