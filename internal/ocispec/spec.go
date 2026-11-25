// Package ocispec builds the OCI runtime spec (config.json) that runsc runs.
package ocispec

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Volume is a host directory bind-mounted into the sandbox.
type Volume struct {
	Host  string `json:"host" yaml:"host"`
	Guest string `json:"guest" yaml:"guest"`
	RW    bool   `json:"rw" yaml:"rw"`
}

// Config is everything gsb lets the user choose about a sandbox.
type Config struct {
	Rootfs      string
	Hostname    string
	Args        []string // the long-lived init process
	Env         []string
	Cwd         string
	UID, GID    uint32
	Volumes     []Volume
	CPUs        float64 // 0 = unlimited
	MemoryBytes int64   // 0 = unlimited
	Pids        int64   // 0 = unlimited
	HostNetwork bool
	CgroupsPath string
}

// A deliberately small capability set. Inside gVisor these are capabilities
// against the Sentry (the user-space kernel), not against the host kernel.
var defaultCaps = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL",
	"CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT", "CAP_MKNOD", "CAP_AUDIT_WRITE", "CAP_SETFCAP",
}

// Process returns a process spec, used both for init and for `runsc exec`.
func Process(args, env []string, cwd string, uid, gid uint32, terminal bool) *specs.Process {
	if cwd == "" {
		cwd = "/"
	}
	return &specs.Process{
		Terminal: terminal,
		Args:     args,
		Env:      env,
		Cwd:      cwd,
		User:     specs.User{UID: uid, GID: gid},
		Capabilities: &specs.LinuxCapabilities{
			Bounding: defaultCaps, Effective: defaultCaps, Permitted: defaultCaps,
		},
		NoNewPrivileges: true,
		Rlimits:         []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 65536, Soft: 65536}},
	}
}

// Build returns the full runtime spec for a sandbox.
func Build(c Config) *specs.Spec {
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
	}
	for _, v := range c.Volumes {
		opts := []string{"rbind", "ro"}
		if v.RW {
			opts = []string{"rbind", "rw"}
		}
		mounts = append(mounts, specs.Mount{Destination: v.Guest, Type: "bind", Source: v.Host, Options: opts})
	}

	namespaces := []specs.LinuxNamespace{
		{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace}, {Type: specs.MountNamespace},
	}
	if !c.HostNetwork {
		namespaces = append(namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace})
	}

	res := &specs.LinuxResources{}
	if c.MemoryBytes > 0 {
		res.Memory = &specs.LinuxMemory{Limit: &c.MemoryBytes}
	}
	if c.CPUs > 0 {
		period := uint64(100000)
		quota := int64(c.CPUs * float64(period))
		res.CPU = &specs.LinuxCPU{Period: &period, Quota: &quota}
	}
	if c.Pids > 0 {
		res.Pids = &specs.LinuxPids{Limit: &c.Pids}
	}

	return &specs.Spec{
		Version:  specs.Version,
		Hostname: c.Hostname,
		// Readonly=false is safe only because runsc is always started with a
		// rootfs overlay (see runsc.Runsc), so writes never reach the cache.
		Root:    &specs.Root{Path: c.Rootfs, Readonly: false},
		Process: Process(c.Args, c.Env, c.Cwd, c.UID, c.GID, false),
		Mounts:  mounts,
		Linux: &specs.Linux{
			Namespaces:  namespaces,
			Resources:   res,
			CgroupsPath: c.CgroupsPath,
			MaskedPaths: []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/sys/firmware"},
		},
	}
}
