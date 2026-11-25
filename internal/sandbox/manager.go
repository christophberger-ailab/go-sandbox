// Package sandbox ties images, OCI specs, runsc and the state store together.
// Every gsb command is a method on Manager.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/christophberger/go-sandbox/internal/archive"
	"github.com/christophberger/go-sandbox/internal/console"
	"github.com/christophberger/go-sandbox/internal/image"
	"github.com/christophberger/go-sandbox/internal/ocispec"
	"github.com/christophberger/go-sandbox/internal/runsc"
	"github.com/christophberger/go-sandbox/internal/store"
)

type Manager struct {
	Images *image.Store
	Store  *store.Store
	Runsc  *runsc.Runsc
}

// New sets up a manager rooted at root (default /var/lib/gsb).
func New(root, runscBin, platform string, extraFlags []string) *Manager {
	return &Manager{
		Images: &image.Store{Dir: filepath.Join(root, "images")},
		Store:  &store.Store{Root: root},
		Runsc: &runsc.Runsc{
			Bin: runscBin, Root: filepath.Join(root, "runsc"),
			Platform: platform, Extra: extraFlags,
		},
	}
}

// Options for creating a sandbox.
type Options struct {
	Name        string
	Image       string
	CPUs        float64
	MemoryBytes int64
	Pids        int64
	Network     string // none (default), sandbox, host
	Volumes     []ocispec.Volume
	Env         []string
	Workdir     string
	Init        []string // long-lived init; default: sleep infinity
	Ephemeral   bool
}

// Fingerprint identifies the options a sandbox was created from, so that
// `gsb up` can tell when a Sandboxfile entry has changed.
func (o Options) Fingerprint() string {
	o.Ephemeral = false
	if o.Network == "" {
		o.Network = "none"
	}
	b, _ := json.Marshal(o)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Create pulls the image (if needed), writes the bundle and starts the sandbox.
func (m *Manager) Create(ctx context.Context, o Options) (*store.Sandbox, error) {
	if err := store.ValidateName(o.Name); err != nil {
		return nil, err
	}
	switch o.Network {
	case "":
		o.Network = "none"
	case "none", "sandbox", "host":
	default:
		return nil, fmt.Errorf("unknown network mode %q (none, sandbox, host)", o.Network)
	}
	if err := os.MkdirAll(m.Images.Dir, 0o700); err != nil {
		return nil, err
	}
	img, err := m.Images.Get(ctx, o.Image)
	if err != nil {
		return nil, err
	}

	unlock, err := m.Store.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if m.Store.Exists(o.Name) {
		return nil, fmt.Errorf("sandbox %q already exists", o.Name)
	}

	cfg := img.Config.Config
	uid, gid := parseUser(cfg.User)
	workdir := firstNonEmpty(o.Workdir, cfg.WorkingDir, "/")
	sb := &store.Sandbox{
		Name: o.Name, ID: "gsb-" + o.Name,
		Image: o.Image, ImageDigest: img.Digest,
		CPUs: o.CPUs, MemoryBytes: o.MemoryBytes, Pids: o.Pids,
		Network: o.Network, Volumes: o.Volumes,
		Env:     mergeEnv(cfg.Env, o.Env),
		Workdir: workdir, UID: uid, GID: gid,
		Ephemeral: o.Ephemeral, Fingerprint: o.Fingerprint(), CreatedAt: time.Now(),
	}
	if err := m.Store.Save(sb); err != nil {
		return nil, err
	}

	initArgs := o.Init
	if len(initArgs) == 0 {
		initArgs = []string{"sleep", "infinity"}
	}
	spec := ocispec.Build(ocispec.Config{
		Rootfs: img.Rootfs, Hostname: o.Name,
		Args: initArgs, Env: sb.Env, Cwd: "/", UID: uid, GID: gid,
		Volumes: o.Volumes, CPUs: o.CPUs, MemoryBytes: o.MemoryBytes, Pids: o.Pids,
		HostNetwork: o.Network == "host",
		CgroupsPath: "/gsb/" + o.Name,
	})
	if err := writeJSON(filepath.Join(m.Store.Bundle(o.Name), "config.json"), spec); err != nil {
		m.Store.Delete(o.Name)
		return nil, err
	}
	if err := m.boot(sb); err != nil {
		m.Store.Delete(o.Name)
		return nil, err
	}
	return sb, nil
}

// boot runs `runsc create` + `runsc start` for an existing record.
func (m *Manager) boot(sb *store.Sandbox) error {
	log, err := os.OpenFile(m.Store.Log(sb.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	if err := m.Runsc.Create(sb.ID, m.Store.Bundle(sb.Name), sb.Network, log); err != nil {
		return err
	}
	if err := m.Runsc.Start(sb.ID); err != nil {
		m.Runsc.Delete(sb.ID, true)
		return err
	}
	return nil
}

// Start boots a stopped sandbox again. Its filesystem starts fresh from
// the image: the rootfs overlay lives only as long as the sandbox does.
func (m *Manager) Start(name string) error {
	unlock, err := m.Store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	sb, err := m.Store.Load(name)
	if err != nil {
		return err
	}
	if st := m.Runsc.State(sb.ID).Status; st == "running" {
		return nil
	} else if st != "absent" {
		m.Runsc.Delete(sb.ID, true)
	}
	return m.boot(sb)
}

// Stop kills all processes and tears down the gVisor sandbox, keeping the record.
func (m *Manager) Stop(name string) error {
	unlock, err := m.Store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	sb, err := m.Store.Load(name)
	if err != nil {
		return err
	}
	return m.teardown(sb)
}

func (m *Manager) teardown(sb *store.Sandbox) error {
	if m.Runsc.State(sb.ID).Status == "absent" {
		return nil
	}
	_ = m.Runsc.Kill(sb.ID, "KILL")
	return m.Runsc.Delete(sb.ID, true)
}

// Remove stops the sandbox (if running) and deletes its record and logs.
func (m *Manager) Remove(name string) error {
	unlock, err := m.Store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	sb, err := m.Store.Load(name)
	if err != nil {
		return err
	}
	if err := m.teardown(sb); err != nil {
		return err
	}
	return m.Store.Delete(name)
}

// Status is a sandbox record plus its live runsc status.
type Status struct {
	*store.Sandbox
	State string
}

func (m *Manager) List() ([]Status, error) {
	sbs, err := m.Store.List()
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(sbs))
	for _, sb := range sbs {
		st := m.Runsc.State(sb.ID).Status
		if st == "absent" {
			st = "stopped"
		}
		out = append(out, Status{Sandbox: sb, State: st})
	}
	return out, nil
}

// ExecOptions for running a command inside a sandbox.
type ExecOptions struct {
	Args    []string
	Env     []string
	Workdir string
	TTY     bool
	Detach  bool // output goes to the sandbox log
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// Exec runs a command in a running sandbox and returns its exit code.
func (m *Manager) Exec(name string, o ExecOptions) (int, error) {
	sb, err := m.Store.Load(name)
	if err != nil {
		return -1, err
	}
	if st := m.Runsc.State(sb.ID).Status; st != "running" {
		return -1, fmt.Errorf("sandbox %s is not running (state: %s); try: gsb start %s", name, st, name)
	}
	if len(o.Args) == 0 {
		o.Args = []string{"/bin/sh"}
	}
	if o.Detach {
		o.TTY = false
	}

	proc := ocispec.Process(o.Args, mergeEnv(sb.Env, o.Env),
		firstNonEmpty(o.Workdir, sb.Workdir), sb.UID, sb.GID, o.TTY)
	pf, err := os.CreateTemp("", "gsb-process-*.json")
	if err != nil {
		return -1, err
	}
	defer os.Remove(pf.Name())
	if err := json.NewEncoder(pf).Encode(proc); err != nil {
		return -1, err
	}
	pf.Close()

	if o.Detach {
		log, err := os.OpenFile(m.Store.Log(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return -1, err
		}
		defer log.Close()
		cmd := m.Runsc.ExecCmd(sb.ID, pf.Name(), "", true)
		cmd.Stdout, cmd.Stderr = log, log
		return 0, cmd.Run()
	}

	if !o.TTY {
		cmd := m.Runsc.ExecCmd(sb.ID, pf.Name(), "", false)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, o.Stdout, o.Stderr
		return exitCode(cmd.Run())
	}

	sock, err := console.NewSocket()
	if err != nil {
		return -1, err
	}
	defer sock.Close()
	cmd := m.Runsc.ExecCmd(sb.ID, pf.Name(), sock.Path, false)
	cmd.Stderr = os.Stderr // runsc's own errors
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	master, err := sock.Receive(10 * time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return -1, fmt.Errorf("waiting for sandbox PTY: %w", err)
	}
	done := console.Attach(master)
	code, err := exitCode(cmd.Wait())
	done()
	return code, err
}

// Run is the ephemeral path: create, exec, remove.
func (m *Manager) Run(ctx context.Context, o Options, e ExecOptions) (int, error) {
	if o.Name == "" {
		o.Name = fmt.Sprintf("run-%d", time.Now().UnixNano()%1e9)
	}
	o.Ephemeral = true
	if _, err := m.Create(ctx, o); err != nil {
		return -1, err
	}
	defer m.Remove(o.Name)
	return m.Exec(o.Name, e)
}

// Logs copies the sandbox log to w, optionally following it like tail -f.
func (m *Manager) Logs(ctx context.Context, name string, follow bool, w io.Writer) error {
	if !m.Store.Exists(name) {
		return fmt.Errorf("no such sandbox: %s", name)
	}
	f, err := os.Open(m.Store.Log(name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// CopyIn copies a host file or directory into the sandbox. The sandbox's
// writes live in its in-sandbox overlay, so this streams a tar through
// `runsc exec tar -x` (the image needs tar).
func (m *Manager) CopyIn(name, hostSrc, guestDir string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(archive.Tar(pw, hostSrc)) }()
	var stderr strings.Builder
	code, err := m.Exec(name, ExecOptions{
		Args:  []string{"tar", "-x", "-f", "-", "-C", guestDir},
		Stdin: pr, Stdout: io.Discard, Stderr: &stderr,
	})
	pr.Close()
	if err == nil && code != 0 {
		err = fmt.Errorf("tar in sandbox exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return err
}

// CopyOut copies a file or directory out of the sandbox into hostDir.
func (m *Manager) CopyOut(name, guestSrc, hostDir string) error {
	guestSrc = filepath.Clean(guestSrc)
	pr, pw := io.Pipe()
	var stderr strings.Builder
	errc := make(chan error, 1)
	go func() {
		// Never trust owners, devices or setuid bits from the sandbox.
		err := archive.Untar(pr, hostDir, false)
		// On error, keep draining so tar in the sandbox doesn't block on a
		// full pipe and hang `gsb cp`.
		io.Copy(io.Discard, pr)
		errc <- err
	}()
	code, err := m.Exec(name, ExecOptions{
		Args:   []string{"tar", "-c", "-f", "-", "-C", filepath.Dir(guestSrc), filepath.Base(guestSrc)},
		Stdout: pw, Stderr: &stderr,
	})
	pw.Close()
	if uerr := <-errc; err == nil {
		err = uerr
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("tar in sandbox exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return err
}

// --- helpers ---

func exitCode(err error) (int, error) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// IsTerminal reports whether stdin and stdout are both terminals.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// mergeEnv overlays KEY=VALUE pairs from extra onto base.
func mergeEnv(base, extra []string) []string {
	out := append([]string{}, base...)
	idx := map[string]int{}
	for i, kv := range out {
		idx[strings.SplitN(kv, "=", 2)[0]] = i
	}
	for _, kv := range extra {
		k := strings.SplitN(kv, "=", 2)[0]
		if i, ok := idx[k]; ok {
			out[i] = kv
		} else {
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	if _, ok := idx["PATH"]; !ok {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return out
}

// parseUser handles numeric "uid[:gid]". User names would need /etc/passwd
// lookup in the rootfs; the prototype falls back to root for those.
func parseUser(u string) (uint32, uint32) {
	parts := strings.SplitN(u, ":", 2)
	uid, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0
	}
	gid := uid
	if len(parts) == 2 {
		if g, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
			gid = g
		}
	}
	return uint32(uid), uint32(gid)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
