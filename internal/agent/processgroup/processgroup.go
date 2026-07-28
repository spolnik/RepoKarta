// Package processgroup owns provider subprocess trees rather than only their
// immediate CLI shim.
package processgroup

import (
	"bytes"
	"os/exec"
	"sync"
)

// Group is an operating-system process-tree lifetime handle.
type Group struct {
	kill func() error
}

// Buffer is safe for a running subprocess writer and concurrent diagnostics.
type Buffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *Buffer) Write(content []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(content)
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// Kill terminates the full provider process tree.
func (g *Group) Kill() error {
	if g == nil || g.kill == nil {
		return nil
	}
	return g.kill()
}

// Configure prepares command before Start. Attach must be called after Start.
func Configure(command *exec.Cmd) {
	configure(command)
}

// Attach creates the lifetime handle for a started command.
func Attach(command *exec.Cmd) (*Group, error) {
	return attach(command)
}
