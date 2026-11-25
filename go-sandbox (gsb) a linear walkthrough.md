# go-sandbox (gsb): a linear walkthrough

Nov 25, 2026 · @Christoph Berger

## Overview

`gsb` is a daemonless Go CLI that gives you microsandbox-style `create` / `exec` / `rm` commands on top of gVisor's `runsc`. Each command pulls an OCI image, writes an OCI bundle, and shells out to `runsc`. State lives in files under `/var/lib/gsb`. The whole thing is about 2,000 lines of Go in one binary and eight internal packages. It is at commit `0ae085c` ("Add gsb: microsandbox-style CLI prototype for gVisor"), the only commit so far.

The README calls it a prototype: it builds, unit tests pass, and the command flow was checked against a stub `runsc`, but it has not yet run against a real `runsc`.

| Path | Lines | Role |
| --- | --- | --- |
| `cmd/gsb/main.go` | 467 | Cobra CLI: flags, one function per command, exit-code plumbing |
| `internal/sandbox` | 518 + 37 test | `Manager`: every command is a method on it; flag parsers |
| `internal/image` | 186 | Pull images with go-containerregistry, cache one rootfs per digest |
| `internal/archive` | 163 + 57 test | Tar in and out, confined to a root with securejoin |
| `internal/ocispec` | 112 | Build the OCI `config.json` and exec `process.json` |
| `internal/runsc` | 117 | Thin wrapper that builds `runsc` command lines |
| `internal/console` | 112 | Console socket that receives the PTY master, raw-mode terminal bridge |
| `internal/store` | 121 | Sandbox records, bundle and log paths, a global flock |
| `internal/project` | 107 | `Sandboxfile` (YAML) parsing for `up` / `down` / `script` |

The module path is `github.com/christophberger/go-sandbox` and it targets Go 1.26. Direct dependencies are cobra (CLI), go-containerregistry (image pulls), opencontainers/runtime-spec (spec types), filepath-securejoin (safe extraction), `x/sys` and `x/term` (sockets, terminal), and yaml.v3 (Sandboxfile).

**Reading order.** This walkthrough starts at `main.go`, then reads the leaf packages in the order `gsb create` touches them: store, image and archive, ocispec, runsc, console. It then reads `internal/sandbox`, where they all meet, and ends with project mode and the tests.

&#91;embedded content: gsb package map · 8 internal packages\]

Arrows point from caller to callee. Dashed boxes are outside the gsb binary: the state directory and the `runsc` process, which hands the PTY back to `console` for interactive `exec`.

## 1. The entry point: `cmd/gsb/main.go`

`main.go` is pure wiring: it parses flags with cobra, turns them into option structs, and calls one `sandbox.Manager` method per command. No sandbox logic lives here.

**Global settings.** Three package-level variables default from the environment, then cobra persistent flags can override them:

```go
var (
	flagRoot     = envOr("GSB_ROOT", "/var/lib/gsb")
	flagRunsc    = envOr("GSB_RUNSC", "runsc")
	flagPlatform = envOr("GSB_PLATFORM", "systrap")
)

func manager() *sandbox.Manager {
	return sandbox.New(flagRoot, flagRunsc, flagPlatform, strings.Fields(os.Getenv("GSB_RUNSC_FLAGS")))
}
```

Every command builds a fresh `Manager` through `manager()`. `GSB_RUNSC_FLAGS` is split on whitespace and appended to every `runsc` call, which is the escape hatch for `--debug` or `--ignore-cgroups`.

**`main()`** wraps everything in `signal.NotifyContext` for SIGINT and SIGTERM. That context only reaches `create`, `run`, `pull`, `up` (image pulls) and `logs -f` (to stop following). `exec` does not take it.

**Exit codes.** `exitErr` is an `int` that satisfies `error`. `exitWith(code, err)` turns a non-zero exit code from the sandboxed process into an `exitErr`, and `main` unwraps it with `os.Exit(int(ec))`. So `gsb exec dev -- false` exits 1 without printing a `gsb:` error line. `SilenceUsage` and `SilenceErrors` keep cobra from printing its own noise.

**Shared flag sets.** Two small structs avoid repeating flag definitions:

- `createFlags` holds `--image`, `--cpus`, `-m`, `--pids` (default 512), `--net` (default `none`), `-v`, `-e`, `-w`. Its `options(name)` calls `sandbox.ParseMemory` and `sandbox.ParseVolume` (relative host paths resolve against the current directory) and returns a `sandbox.Options`.
- `execFlags` holds `-e`, `-w`, `-t`, `-T`, `-d`. PTY choice is `(tty || IsTerminal()) && !noTTY`: auto-detected, `-t` forces it on, `-T` forces it off.

**Commands.** Each command function returns a `*cobra.Command` whose `RunE` calls the manager:

| Command | Calls | Notes |
| --- | --- | --- |
| `create NAME -i IMAGE` | `Manager.Create` | Prints the name on success |
| `start` / `stop` / `rm` NAME... | `Start` / `Stop` / `Remove` via `each()` | `rm` has alias `delete` |
| `ls` | `Manager.List` | `tabwriter` table; memory shown in MiB |
| `exec NAME [-- CMD]` | `Manager.Exec` | Exit code passed through |
| `run IMAGE [-- CMD]` | `Manager.Run` | `-e`/`-w` go to the create options; no `-d` |
| `logs NAME [-f]` | `Manager.Logs` |  |
| `cp SRC DST` | `CopyIn` / `CopyOut` | `splitRemote` decides direction |
| `pull IMAGE` / `images` | `Images.Pull` / `Images.List` | Bypasses the sandbox layer |
| `up` / `down` \[NAME...\] | `project.Find`, then `Create`/`Start` or `Remove` | `up` starts an existing sandbox instead of recreating it |
| `script SB SCRIPT [-- ARGS]` | `Manager.Exec` with `/bin/sh -c` | See section 8 |

**Helpers.** `each(names, fn)` runs `fn` for every name, prints the name on success, prints `gsb: NAME: err` on failure, and returns exit code 1 at the end if anything failed. `splitRemote` parses `NAME:PATH` for `cp`; a leading `/` or `.` always means a host path, so `./a:b` is never mistaken for a sandbox.

## 2. State on disk: `internal/store`

`store` is gsb's whole database: one directory per sandbox and a single lock file. The package comment states the layout:

```
<root>/sandboxes/<name>/sandbox.json   the Sandbox record
<root>/sandboxes/<name>/bundle/        OCI bundle (config.json)
<root>/sandboxes/<name>/output.log     init + detached exec output
<root>/lock                            global flock
```

**The record.** `store.Sandbox` is what `create` decided: name, runsc container ID (`gsb-<name>`), image ref and digest, limits, network mode, volumes, the merged environment, default workdir, UID/GID, an `Ephemeral` flag and `CreatedAt`. It deliberately holds no status field. Live status always comes from `runsc state`, so the record can't go stale.

**Names.** `ValidateName` enforces `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`. Because the name becomes a path component, a cgroup path and part of the runsc ID, this regex is the only thing stopping `../` in a name from reaching the filesystem.

**Paths.** `Dir`, `Bundle` and `Log` are one-line path builders. The manager asks the store for these paths instead of joining them itself.

**Locking.** `Lock()` opens `<root>/lock` and takes an exclusive `flock`, returning a release function:

```go
if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { ... }
return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
```

It is one global lock, not per sandbox. `Create`, `Start`, `Stop` and `Remove` hold it; `Exec`, `List` and `Logs` don't. This is the podman-style "daemonless" coordination the README mentions.

**CRUD.** `Save` creates the bundle directory and writes `sandbox.json` with mode 0600. `Load` turns a missing file into `no such sandbox: NAME`. `List` reads every subdirectory, silently skips ones that fail to load, and sorts by name. `Delete` is `os.RemoveAll` on the sandbox directory, which also removes its bundle and log.

## 3. Images: `internal/image` and `internal/archive`

An image becomes one flattened, unpacked rootfs per manifest digest, shared read-only by every sandbox made from it. `image` handles the registry and the cache; `archive` does the tar work for both images and `gsb cp`.

### The cache layout

```
images/index.json               ref -> digest hex
images/<digest-hex>/rootfs/     unpacked, flattened layers
images/<digest-hex>/image.json  Image{Ref, Digest, Rootfs, Config}
```

`Image.Config` is the full OCI `v1.ConfigFile`. The sandbox layer later reads `Config.Config.User`, `.Env` and `.WorkingDir` from it.

### `Get` and `Pull`

`Get(ref)` looks the ref up in `index.json` and only calls `Pull` on a miss. So `gsb create` reuses whatever digest a tag pointed to at its last pull. `gsb pull` always goes to the registry.

`Pull` uses go-containerregistry with the default keychain (Docker credentials) and asks for `linux/<GOARCH>`. Then:

1. Read the manifest digest; the target directory is `images/<digest-hex>`.
2. If `image.json` already exists there, skip unpacking.
3. Otherwise make a temp dir `images/.unpack-*`, call `mutate.Extract(remoteImg)` to get one tar stream with all layers flattened and whiteouts applied, and feed it to `archive.Untar(rc, rootfs, os.Geteuid() == 0)`. Owners and device nodes are restored only when running as root.
4. Write `image.json` into the temp dir and `os.Rename` it into place. A concurrent pull that lost the race gets `EEXIST`/`ENOTEMPTY`, which `os.IsExist` accepts; the deferred `RemoveAll` cleans up the loser's temp dir.
5. Record `ref -> digest` in `index.json` with a write-to-temp-and-rename.

`readMeta` always recomputes `Rootfs` from the directory it actually read. That is defensive: the stored path already points at the final digest directory.

### `archive.Untar`: confined extraction

The point of `archive` is that a malicious layer (or a malicious sandbox during `cp`) must not write outside the target root. `extractEntry` resolves only the parent directory through securejoin and never follows the final component (error checks trimmed):

```go
parent, err := securejoin.SecureJoin(root, filepath.Dir(filepath.Clean("/"+hdr.Name)))
base := filepath.Base(hdr.Name)
if base == "/" || base == "." {
	return nil // the root itself
}
target := filepath.Join(parent, base)
```

Then it switches on the entry type:

- **Dir**: replace a non-directory in the way, then `Mkdir`.
- **Regular file**: remove whatever is there, then open with `O_CREATE|O_EXCL|O_NOFOLLOW` so a planted symlink can't redirect the write.
- **Symlink**: `RemoveAll` the target, then create the link as-is. Its target is not checked, which is fine because later entries resolve through securejoin.
- **Hard link**: the link source is resolved with securejoin too.
- **Char, block, FIFO**: created with `mknod` only when `preserveOwner` is set; skipped otherwise.
- Anything else (PAX headers and so on) is ignored.

Finally it `Lchown`s if preserving owners, then `chmod`s everything except symlinks. The comment notes the order matters: `chown` clears setuid and setgid bits, so `chmod` has to come second.

Section 9 covers one case this logic misses: an entry whose last path element is `..`.

### `archive.Tar`

`Tar(w, src)` walks `src` and names entries relative to `src`'s parent, so `tar -x -C dest` recreates `basename(src)` under `dest`. Symlinks are stored as links, and only regular files get content. This is the host-to-sandbox half of `gsb cp`.

## 4. The OCI spec: `internal/ocispec`

`ocispec` turns gsb's user-facing choices into the `config.json` that `runsc create` reads. It has two builders: `Process` for a single process and `Build` for the whole spec.

**`Config`** is the input: rootfs path, hostname, init args, env, cwd, UID/GID, volumes, CPU/memory/pids limits, a `HostNetwork` bool and a cgroups path. `Volume{Host, Guest, RW}` is defined here too, and the store and project packages reuse it.

**`Process`** is used twice: for the init process inside `Build`, and on its own for every `gsb exec` (written to a temp `process.json`). Every process gets the same hardening:

- a fixed set of 13 capabilities (`CAP_CHOWN`, `CAP_DAC_OVERRIDE`, `CAP_SETUID`, `CAP_NET_BIND_SERVICE` and others) as bounding, effective and permitted sets; no `CAP_SYS_ADMIN`, no `CAP_NET_RAW`
- `NoNewPrivileges: true`
- `RLIMIT_NOFILE` of 65536
- cwd defaulting to `/`

The comment on `defaultCaps` makes the gVisor point: these are capabilities against the Sentry, gVisor's user-space kernel, not against the host kernel.

**`Build`** assembles the rest:

- **Mounts.** `/proc`, a 64 MiB tmpfs `/dev`, a fresh `devpts` (`newinstance`, needed for PTYs), a 64 MiB `/dev/shm`, read-only `/sys` and a tmpfs `/tmp`. Each volume becomes an `rbind` mount, `ro` unless `RW` is set.
- **Namespaces.** PID, IPC, UTS and mount always. A network namespace is added unless `HostNetwork` is set, so `--net host` means "no netns" and `none`/`sandbox` both get one.
- **Resources.** Memory as a byte limit. CPU as a CFS quota over a 100 ms period, so `--cpus 1.5` becomes quota 150000. Pids as a limit. Zero means "leave unset".
- **Masked paths.** `/proc/kcore`, `/proc/keys`, `/proc/timer_list` and `/sys/firmware`.

One line carries a safety argument that spans packages:

```go
// Readonly=false is safe only because runsc is always started with a
// rootfs overlay (see runsc.Runsc), so writes never reach the cache.
Root: &specs.Root{Path: c.Rootfs, Readonly: false},
```

The rootfs points straight at the shared image cache. It stays unmodified only because every `runsc` call carries `--overlay2 root:self` (next section).

## 5. Talking to gVisor: `internal/runsc`

`runsc` is a thin command-line builder. gsb shells out instead of linking gVisor because, as the package comment says, the CLI is runsc's supported, versioned interface, and runsc daemonizes each sandbox so it outlives the `gsb` process.

**Global flags.** `Runsc{Bin, Root, Platform, Extra}` holds settings that must be identical on every call for a given container, notably `--root`. `global()` builds them:

```go
args := []string{
	"--root", r.Root,
	"--platform", r.Platform,
	"--overlay2", "root:self",
}
args = append(args, r.Extra...)
```

`--overlay2 root:self` puts a copy-on-write overlay over the rootfs, backed by a file inside the rootfs directory. That is the counterpart of `Readonly: false` in the spec. It also explains the README's warning: the overlay dies with the sandbox, so `stop` then `start` gives you a fresh filesystem.

**Subcommands.**

| Method | runsc call | Notes |
| --- | --- | --- |
| `Create(id, bundle, network, log)` | `--network N create --bundle B ID` | Init's stdout and stderr go to `output.log` |
| `Start(id)` | `start ID` |  |
| `ExecCmd(id, file, sock, detach)` | `exec --process F [--console-socket S] [--detach] ID` | Returns an unstarted `*exec.Cmd`; the caller wires stdio |
| `Kill(id, sig)` | `kill --all ID SIG` |  |
| `Delete(id, force)` | `delete [--force] ID` |  |
| `State(id)` | `state ID` | A failed call means `"absent"` |

`run()` executes a command and folds the full argv and stderr into the error, which makes failures easy to read. `State` never returns an error: a non-zero exit becomes `Status: "absent"` and unparseable JSON becomes `"unknown"`. The manager relies on `"absent"` to mean "not running, nothing to clean up".

## 6. Interactive terminals: `internal/console`

`console` makes `gsb exec` feel like `ssh`. It uses the runc/runsc console-socket protocol: gsb listens on a Unix socket, runsc creates a PTY inside the sandbox, and runsc sends the PTY master's file descriptor over the socket with `SCM_RIGHTS`.

**`NewSocket`** makes a private temp directory (`gsb-console-*`) and listens on `console.sock` inside it. `Close` removes the whole directory.

**`Receive(timeout)`** sets an accept deadline (the manager uses 10 seconds), accepts one connection, and reads one message with room for exactly one fd of out-of-band data (error checks trimmed):

```go
oob := make([]byte, unix.CmsgSpace(4))
_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
fds, err := unix.ParseUnixRights(&msgs[0])
return os.NewFile(uintptr(fds[0]), "pty-master"), nil
```

**`Attach(master)`** bridges the user's terminal to that fd and returns a `done` function:

1. Put stdin in raw mode if it's a terminal, and remember how to restore it.
2. Copy the current window size to the master with `TIOCGWINSZ`/`TIOCSWINSZ`, and do it again on every `SIGWINCH`.
3. Start two goroutines: stdin to master, and master to stdout.
4. `done()` waits up to 500 ms for output to drain, stops the `SIGWINCH` handler, closes the master and restores the terminal.

The 500 ms cap exists because a background process inside the sandbox can keep the PTY slave open. Without it, gsb would hang after the shell exits. The stdin goroutine is never stopped; it stays blocked on `os.Stdin` until gsb exits, which is fine for a one-shot CLI but would leak in a long-running daemon.

## 7. Where it all meets: `internal/sandbox`

`sandbox.Manager` is the core of gsb: every command is one of its methods, and it imports every other internal package except project. `New(root, runsc, platform, extra)` wires an `image.Store` at `<root>/images`, a `store.Store` at `<root>`, and a `runsc.Runsc` whose state directory is `<root>/runsc`.

### `Create`

This is the longest path through the code, so it's worth reading in order:

1. Validate the name and the network mode (`""` becomes `none`; anything other than `none`, `sandbox` or `host` is an error).
2. `Images.Get(ctx, image)`, pulling if needed. This happens **before** taking the lock, so a slow pull doesn't block other gsb commands.
3. Take the global lock and refuse if the name exists.
4. Read the image config. `parseUser` accepts only numeric `uid[:gid]`; named users fall back to root. Workdir is the first non-empty of `-w`, the image's `WorkingDir`, and `/`. Env is `mergeEnv(image env, -e flags)`.
5. Save the `store.Sandbox` record.
6. Build the spec with `ocispec.Build`. Init is `sleep infinity` unless `Options.Init` is set; hostname is the sandbox name; cgroups path is `/gsb/<name>`. Write it as `bundle/config.json`.
7. `boot(sb)`: open `output.log` for append, `runsc create`, then `runsc start`. If `start` fails, `boot` force-deletes the runsc container; if `boot` fails, `Create` deletes the record.

Note the design choice in step 6: the sandbox's init does nothing but sleep. All real work arrives later through `runsc exec`, which is what lets `create` and `exec` be separate commands.

### `Start`, `Stop`, `Remove`, `List`

All except `List` take the lock and load the record first.

- `Stop` calls `teardown`: if runsc reports `absent`, nothing to do; otherwise `kill --all KILL` (error ignored) and `delete --force`. The record stays.
- `Start` returns early if already `running`, force-deletes any leftover runsc state, then calls `boot` again. The filesystem is fresh, because the overlay died with the old sandbox.
- `Remove` is `teardown` plus `Store.Delete`.
- `List` merges each record with `runsc state`, showing `absent` as `stopped`.

### `Exec`

`Exec` does one setup step, then picks one of three output modes:

1. **Setup.** Load the record and require status `running` (the error suggests `gsb start`). An empty command means `/bin/sh`. `-d` forces TTY off. Build a process spec with the record's UID/GID, merged env and workdir, and write it to a temp `gsb-process-*.json` that is removed on return.
2. **Detached.** Open `output.log`, run `runsc exec --detach` with stdout and stderr pointed at the log, and return 0. The process keeps running inside the sandbox.
3. **Plain pipes.** No TTY: wire the caller's stdin, stdout and stderr to `runsc exec` and return the exit code through `exitCode`, which unwraps `*exec.ExitError`.
4. **PTY.** Create a console socket, start `runsc exec --console-socket`, wait up to 10 s for the master fd, then `console.Attach` until `runsc exec` exits.

### `Run`, `Logs`, `CopyIn`, `CopyOut`

- `Run` names the sandbox `run-<nanoseconds mod 1e9>` if unnamed, marks it ephemeral, calls `Create`, defers `Remove`, and returns `Exec`'s exit code.
- `Logs` copies `output.log` to stdout. With `-f` it polls every 250 ms until the context is cancelled.
- `CopyIn` streams `archive.Tar(hostSrc)` through an `io.Pipe` into `Exec("tar -x -f - -C guestDir")`. Files must go through `exec` because the sandbox's writes live in its in-memory overlay, not on the host. The image therefore needs `tar`.
- `CopyOut` runs `tar -c` inside the sandbox and feeds its stdout to `archive.Untar(pr, hostDir, false)`. The `false` means owners and device nodes from the sandbox are never restored on the host.

### Helpers and parsers

- `mergeEnv` overlays `KEY=VALUE` pairs and keeps the original order, so a user `-e PATH=...` replaces the image's `PATH` in place. If no `PATH` exists at all, it appends a standard one.
- `ParseMemory` (in `parse.go`) accepts `k`, `m`, `g` suffixes and decimals like `1.5G`. A trailing `b` is stripped but not understood, so `512mb` parses as 512 bytes (see section 9).
- `ParseVolume` parses `host:guest[:ro|rw]`. It resolves relative host paths against a base directory (the cwd for flags, the Sandboxfile's directory for project mode), requires an absolute guest path, and requires the host path to exist. Volumes are read-only by default.

## 8. Project mode: `internal/project`

A `Sandboxfile` is a small compose file: named sandboxes plus named scripts, so a repo can declare its environment and `gsb up` builds it. `Sandboxfile.example` defines two: `dev` (Go 1.23, 2 CPUs, 2g, host network, the repo mounted read-write at `/src`, scripts `build`/`test`/`vet`) and `play` (no network, 1 CPU, 512m, 128 pids, `./snippets` mounted read-only).

**Finding and loading.** `Find()` walks up from the current directory until it finds `Sandboxfile` or `Sandboxfile.yaml`, like git finds `.git`. `Load` unmarshals YAML into `Project{Dir, Sandboxes map[string]Def}`, where `Dir` is the file's directory. `Names()` returns the sandbox names sorted.

**`Def` to `Options`.** `Options(name)` reuses the sandbox package's parsers, so a Sandboxfile entry and command-line flags are validated the same way. Two differences: volume host paths resolve against the Sandboxfile's directory, not the cwd, and the `env` map is flattened to sorted `KEY=VALUE` strings so the order is stable.

**The three commands** live in `main.go`:

- `up [NAME...]` creates each named sandbox, or all of them. A sandbox that already exists is **started**, not recreated, so edits to the Sandboxfile only take effect after `down`.
- `down [NAME...]` removes them, skipping names with no record.
- `script SB SCRIPT [-- ARGS]` looks up the script body and runs it through the shell, passing extra args as positional parameters:

```go
// sh -c 'script "$@"' gsb args...  passes extra args through.
cmd := append([]string{"/bin/sh", "-c", body + ` "$@"`, "gsb"}, args[2:]...)
```

`"gsb"` becomes `$0`, and the extra args become `$1...`. So `gsb script dev test -- -run TestFoo` runs `go test ./... -run TestFoo`.

## 9. Tests, verification and findings

The unit tests cover only the two packages with pure logic. Reading the code turned up one real security bug in extraction, which I reproduced, plus several smaller issues.

**Tests in the repo.**

- `archive_test.go`: `TestUntarStaysInsideRoot` plants a symlink to a directory outside the root, writes through it, and writes `../../pwned2`; the test checks that nothing lands outside and that ../../pwned2 is clamped into the root. `TestTarRoundTrip` checks that a directory with a symlink survives `Tar` then `Untar`.
- `parse_test.go`: `ParseMemory` sizes, `ParseVolume` defaults (read-only, relative host path, absolute guest path) and `mergeEnv` ordering.
- Nothing tests `store`, `image`, `ocispec`, `runsc`, `console`, `project` or the `Manager`. The README says the command flow was checked by hand against a stub `runsc` and that nothing has run against a real one yet.

**What I ran.** The Go 1.26 toolchain and the module proxy were unreachable from my environment, so I couldn't build the whole binary. I did build `internal/archive` alone with Go 1.24.7, `filepath-securejoin` v0.7.0 and `x/sys` v0.26.0 (not the pinned v0.50.0, which needs Go 1.26). Both repo tests pass.

### Finding 1: a `..` entry escapes the extraction root (high)

`extractEntry` cleans the name to find the parent directory, but takes the base name from the **raw** name:

```go
parent, err := securejoin.SecureJoin(root, filepath.Dir(filepath.Clean("/"+hdr.Name)))
base := filepath.Base(hdr.Name)        // ".." for an entry named "x/.."
target := filepath.Join(parent, base)  // root/.. = the directory above root
```

For an entry named `x/..`, the cleaned path is `/`, so `parent` is the root, and `target` becomes the directory **above** the root. The `base == "."` check doesn't catch `..`. I added two tests to a scratch copy, and both reproduce the bug:

- A **symlink** entry `x/..` runs `os.RemoveAll` on the directory above the root, then replaces it with a symlink. The next entry is then written under the symlink's target, outside the root. In the test, a file next to the root was deleted and a later file landed in an unrelated directory.
- A **directory** entry `x/..` with mode 0777 `chmod`s the directory above the root to 0777.
- Go's strict mode (`GODEBUG=tarinsecurepath=0`) rejects a bare `..` but still lets `x/..` through.

The path most exposed is `gsb cp NAME:PATH DEST`. Code inside the sandbox controls that tar stream: it can replace `tar` in its own overlay. gsb runs as root, so `gsb cp dev:/out /srv/out` would let the sandbox delete `/srv` on the host. With a relative destination like `./out`, `RemoveAll(".")` is refused by Go, but the `chmod` case still hits the current directory. Image pulls go through the same function; I did not check whether go-containerregistry's `mutate.Extract` normalizes names first.

The fix is to derive the base from the cleaned path. With it, all four tests pass:

```diff
-	parent, err := securejoin.SecureJoin(root, filepath.Dir(filepath.Clean("/"+hdr.Name)))
+	clean := filepath.Clean("/" + hdr.Name)
+	if clean == "/" {
+		return nil // the root itself
+	}
+	parent, err := securejoin.SecureJoin(root, filepath.Dir(clean))
 	if err != nil {
 		return err
 	}
-	base := filepath.Base(hdr.Name)
-	if base == "/" || base == "." {
-		return nil // the root itself
-	}
+	base := filepath.Base(clean)
```

### Smaller findings (from reading, not run)

| Severity | Where | What happens | Suggested fix |
| --- | --- | --- | --- |
| Medium | `Manager.CopyOut` | If `Untar` fails mid-stream, its goroutine returns and nobody drains the pipe. `tar -c` in the sandbox blocks on stdout and `cp` hangs. | `pr.CloseWithError(err)` when `Untar` returns an error |
| Medium | `ParseMemory` | `-m 512mb` strips the `b`, finds no unit, and sets a 512-byte memory limit. | Strip a trailing `b` before checking for `k`/`m`/`g` |
| Low | `Manager.Create` | If writing `config.json` fails, the record saved just before stays. The next `create` says "already exists". | Delete the record on that error too, as the boot-failure path does |
| Low | `image.Store.setIndex` | The read-modify-write of `index.json` is guarded only by an in-process mutex. Two concurrent `gsb pull`s can drop an entry. | Take the store's flock, or one file per ref |
| Low | `gsb up` | Existing sandboxes are started, not reconciled with an edited Sandboxfile. | Compare the record with the definition, or document `down` then `up` |
| Low | `gsb script` | `"$@"` is appended to the end of the script body, so in `a && b` the args go only to `b`. | Document it, or wrap the body in a shell function |
| Low | `ParseVolume` | Splits on every `:`, so host paths that contain `:` can't be mounted. | Split from the right |

The README's own "Known gaps" list still applies: `--net sandbox` has no uplink without CNI, image `USER` must be numeric, there's no rootless mode or image GC, and the `sleep infinity` init rules out distroless images.
