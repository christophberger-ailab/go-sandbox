// Package runsc is a thin wrapper around the runsc command line.
//
// gsb deliberately shells out to runsc instead of linking gVisor: runsc is
// the supported, versioned interface, and each sandbox keeps running after
// gsb exits because runsc daemonizes the sandbox process itself.
package runsc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runsc holds global flags shared by every runsc invocation. They must be the
// same for all commands that touch a given container (notably --root).
type Runsc struct {
	Bin      string   // path to runsc
	Root     string   // runsc state dir
	Platform string   // systrap (default, no KVM needed) or kvm
	Extra    []string // extra global flags, e.g. from $GSB_RUNSC_FLAGS
}

func (r *Runsc) global(extra ...string) []string {
	args := []string{
		"--root", r.Root,
		"--platform", r.Platform,
		// Copy-on-write overlay over the image rootfs, backed by a file in the
		// rootfs dir. Sandboxes share the read-only image cache safely.
		"--overlay2", "root:self",
	}
	args = append(args, r.Extra...)
	return append(args, extra...)
}

func (r *Runsc) command(global []string, sub ...string) *exec.Cmd {
	return exec.Command(r.Bin, append(global, sub...)...)
}

func run(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	if cmd.Stderr == nil {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Create creates the sandbox and its init container from an OCI bundle.
// The init process inherits log as stdout/stderr. network is "none",
// "sandbox" or "host".
func (r *Runsc) Create(id, bundle, network string, log *os.File) error {
	cmd := r.command(r.global("--network", network), "create", "--bundle", bundle, id)
	cmd.Stdout = log
	cmd.Stderr = log
	return run(cmd)
}

// Start starts the init process of a created container.
func (r *Runsc) Start(id string) error {
	return run(r.command(r.global(), "start", id))
}

// ExecCmd prepares `runsc exec` for a process.json. With consoleSocket set,
// runsc allocates a PTY inside the sandbox and sends its master fd there.
func (r *Runsc) ExecCmd(id, processFile, consoleSocket string, detach bool) *exec.Cmd {
	sub := []string{"exec", "--process", processFile}
	if consoleSocket != "" {
		sub = append(sub, "--console-socket", consoleSocket)
	}
	if detach {
		sub = append(sub, "--detach")
	}
	return r.command(r.global(), append(sub, id)...)
}

// Kill sends a signal to all processes in the container.
func (r *Runsc) Kill(id, signal string) error {
	return run(r.command(r.global(), "kill", "--all", id, signal))
}

// Delete removes the container and tears down the sandbox.
func (r *Runsc) Delete(id string, force bool) error {
	sub := []string{"delete"}
	if force {
		sub = append(sub, "--force")
	}
	return run(r.command(r.global(), append(sub, id)...))
}

// State is the subset of `runsc state` output gsb uses.
type State struct {
	ID     string `json:"id"`
	Status string `json:"status"` // created, running, stopped
	Pid    int    `json:"pid"`
}

// State returns the container state, or Status "absent" if runsc doesn't know it.
func (r *Runsc) State(id string) State {
	var out bytes.Buffer
	cmd := r.command(r.global(), "state", id)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return State{ID: id, Status: "absent"}
	}
	var s State
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		return State{ID: id, Status: "unknown"}
	}
	return s
}
