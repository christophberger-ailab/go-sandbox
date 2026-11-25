# gsb: microsandbox-style CLI for gVisor

`gsb` gives you the `msb create` / `msb exec` workflow on top of
[gVisor](https://gvisor.dev)'s `runsc`. Sandboxes start in about a
container's time, use any OCI image, and run on hosts without KVM or
nested virtualization.

> **Status: prototype.** It builds and its unit tests pass. The command
> flow was checked against a stub `runsc` that records its calls, and
> `gsb pull` was tested against Docker Hub. It has **not** yet been run
> against a real `runsc`. See [What's verified](#whats-verified).

## Quick start

```sh
sudo gsb create dev --image golang:1.23 --cpus 2 -m 1g -v .:/src:rw -w /src
sudo gsb exec dev                    # interactive shell (PTY)
sudo gsb exec dev -- go test ./...   # one-off command, exit code is passed through
sudo gsb exec -d dev -- go run ./srv # background, output in `gsb logs dev`
sudo gsb ls
sudo gsb cp ./main.go dev:/src       # host -> sandbox
sudo gsb cp dev:/src/bin ./out       # sandbox -> host
sudo gsb stop dev && sudo gsb start dev
sudo gsb rm dev

sudo gsb run golang:1.23 -- go version   # ephemeral: create, exec, rm
```

Project mode works with a `Sandboxfile` (see `Sandboxfile.example`):

```sh
sudo gsb up                 # create every sandbox in ./Sandboxfile (recreates changed entries)
sudo gsb script dev test    # run a named script inside "dev"
sudo gsb script dev test -- -run TestFoo   # extra args are the script's "$@"
sudo gsb down
```

## Commands

| Command | What it does | runsc underneath |
|---|---|---|
| `create NAME -i IMAGE` | pull + unpack image, write OCI bundle, boot sandbox with `sleep infinity` as init | `create`, `start` |
| `exec NAME [-- CMD]` | run in sandbox; PTY auto-detected (`-t`/`-T`), `-d` detaches | `exec --process --console-socket` |
| `run IMAGE [-- CMD]` | throwaway sandbox | create + exec + delete |
| `ls` | records + live status | `state` |
| `stop` / `start` | tear down / boot again (filesystem resets) | `kill --all`, `delete` |
| `rm` | stop and delete record | `kill`, `delete --force` |
| `logs [-f]` | init + detached exec output | (file) |
| `cp` | tar stream through `exec tar` | `exec` |
| `pull` / `images` | image cache | none |
| `up` / `down` / `script` | Sandboxfile project mode | as above |

Create flags: `--cpus 1.5`, `-m 512m`, `--pids 512` (the default),
`--net none|sandbox|host` (default `none`), `-v host:guest[:rw]`
(read-only by default), `-e KEY=VAL`, `-w DIR`.

Global settings: `--root` / `$GSB_ROOT` (default `/var/lib/gsb`),
`--runsc` / `$GSB_RUNSC`, `--platform` / `$GSB_PLATFORM` (`systrap` or
`kvm`). Use `$GSB_RUNSC_FLAGS` to pass extra runsc flags, e.g.
`"--debug --debug-log=/tmp/runsc/"` or `"--ignore-cgroups"`.

## Install

Requires Linux (amd64 or arm64), root, and Go ≥ 1.24 (the toolchain
auto-upgrades to the version in `go.mod`).

```sh
# 1. runsc (official release, checksum-verified)
ARCH=$(uname -m)
URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}
wget ${URL}/runsc ${URL}/runsc.sha512
sha512sum -c runsc.sha512
chmod a+rx runsc && sudo mv runsc /usr/local/bin/
runsc --version

# Or the apt repo:
#   curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
#   echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list
#   sudo apt-get update && sudo apt-get install -y runsc

# 2. gsb
go install github.com/christophberger/go-sandbox/cmd/gsb@latest
sudo cp "$(go env GOPATH)/bin/gsb" /usr/local/bin/   # gsb runs as root
```

On bare metal with `/dev/kvm`, `GSB_PLATFORM=kvm` speeds up
syscall-heavy work like `go build`.

## How it works

```
gsb (CLI, daemonless)
 ├─ image   go-containerregistry pull → flattened rootfs in /var/lib/gsb/images/<digest>/rootfs (shared, never written)
 ├─ ocispec config.json: rootfs, mounts, caps, cgroup limits, namespaces
 ├─ runsc   create/start/exec/kill/delete --root /var/lib/gsb/runsc --overlay2 root:self
 ├─ console receives the PTY master over a console socket (SCM_RIGHTS) → raw-mode terminal bridge
 └─ store   /var/lib/gsb/sandboxes/<name>/{sandbox.json,bundle/,output.log} + flock
```

Notes on the design:

- **No daemon (yet).** The design we discussed had a `gsbd` daemon on a
  Unix socket. The prototype doesn't need one: `runsc` already
  daemonizes each sandbox, so `gsb` keeps its state in files and uses a
  flock, the same way podman works without a daemon. A daemon becomes
  useful once you want a remote or HTTP API, non-root clients, or
  networking setup. `internal/sandbox.Manager` is the piece to put
  behind it.
- **Copy-on-write rootfs.** All sandboxes from one image share a single
  unpacked rootfs. `--overlay2 root:self` keeps every write inside the
  sandbox's own overlay, so the cache is never modified. As a result,
  the filesystem is ephemeral: `stop`/`start` resets it. Use `-v` for
  anything that should persist.
- **Secure defaults.** No network, read-only volumes, `pids=512`,
  `noNewPrivileges`, and a reduced capability set. Image extraction is
  confined to its root with securejoin, which is covered by a test
  against symlink, `../` and `x/..` escapes. `cp` out of a sandbox never
  restores owners, device nodes or setuid/setgid bits on the host.

## What's verified

| | |
|---|---|
| ✅ | `go build`, `go vet`, unit tests (archive escape, round trip, parsers, env merge) |
| ✅ | `gsb pull alpine:3.20`: pull and unpack from Docker Hub |
| ✅ | create / ls / exec / logs / stop / run / up / script / down / rm against a stub `runsc` that records calls; exit-code passthrough |
| ❌ | Not run against a real `runsc`. Things to check first: |
| | `runsc exec --console-socket` delivers the PTY as expected for interactive `exec` |
| | `--overlay2 root:self` with several sandboxes sharing one rootfs dir (fallback: `root:memory`) |
| | cgroup limits on your host's cgroup setup (fallback: `GSB_RUNSC_FLAGS=--ignore-cgroups`) |

## Known gaps / next steps

- `--net sandbox` gives gVisor's own netstack, but it has no uplink
  until something creates a veth/netns (CNI). Today that leaves
  `none` (isolated) or `host` (shares the host network stack, weaker).
- Image `USER` must be numeric; named users fall back to root.
- No rootless mode, no image GC, and no `gsb images rm`.
- Init is `sleep infinity`, so the image needs `sleep` (distroless
  images don't have it). A tiny static init binary would fix that.
