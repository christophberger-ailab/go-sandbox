// Command gsb is a microsandbox-style CLI for gVisor sandboxes.
//
//	gsb create dev --image golang:1.23 --cpus 1 --memory 512m
//	gsb exec dev -- go version
//	gsb run golang:1.23 -- go env GOOS
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/christophberger/go-sandbox/internal/image"
	"github.com/christophberger/go-sandbox/internal/project"
	"github.com/christophberger/go-sandbox/internal/sandbox"
)

var (
	flagRoot     = envOr("GSB_ROOT", "/var/lib/gsb")
	flagRunsc    = envOr("GSB_RUNSC", "runsc")
	flagPlatform = envOr("GSB_PLATFORM", "systrap")
)

func manager() *sandbox.Manager {
	return sandbox.New(flagRoot, flagRunsc, flagPlatform, strings.Fields(os.Getenv("GSB_RUNSC_FLAGS")))
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:           "gsb",
		Short:         "Microsandbox-style CLI for gVisor (runsc) sandboxes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&flagRoot, "root", flagRoot, "state directory ($GSB_ROOT)")
	pf.StringVar(&flagRunsc, "runsc", flagRunsc, "runsc binary ($GSB_RUNSC)")
	pf.StringVar(&flagPlatform, "platform", flagPlatform, "gVisor platform: systrap or kvm ($GSB_PLATFORM)")

	root.AddCommand(
		createCmd(ctx), startCmd(), stopCmd(), rmCmd(), lsCmd(),
		execCmd(), runCmd(ctx), logsCmd(ctx), cpCmd(),
		pullCmd(ctx), imagesCmd(),
		upCmd(ctx), downCmd(), scriptCmd(),
	)
	if err := root.Execute(); err != nil {
		var ec exitErr
		if asExit(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "gsb:", err)
		os.Exit(1)
	}
}

// exitErr carries a sandboxed process's exit code out through cobra.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func asExit(err error, target *exitErr) bool {
	e, ok := err.(exitErr)
	*target = e
	return ok
}
func exitWith(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return exitErr(code)
	}
	return nil
}

// --- shared flags ---

type createFlags struct {
	image, memory, network, workdir string
	cpus                            float64
	pids                            int64
	volumes, env                    []string
}

func (f *createFlags) register(c *cobra.Command, withImage bool) {
	fl := c.Flags()
	if withImage {
		fl.StringVarP(&f.image, "image", "i", "", "OCI image, e.g. golang:1.23 (required)")
	}
	fl.Float64Var(&f.cpus, "cpus", 0, "CPU limit, e.g. 1.5 (0 = unlimited)")
	fl.StringVarP(&f.memory, "memory", "m", "", "memory limit, e.g. 512m, 2g")
	fl.Int64Var(&f.pids, "pids", 512, "max processes (0 = unlimited)")
	fl.StringVar(&f.network, "net", "none", "network: none, sandbox (gVisor netstack in a netns you provide), host")
	fl.StringArrayVarP(&f.volumes, "volume", "v", nil, "bind mount host:guest[:ro|rw] (read-only by default)")
	fl.StringArrayVarP(&f.env, "env", "e", nil, "environment KEY=VALUE")
	fl.StringVarP(&f.workdir, "workdir", "w", "", "default working directory for exec")
}

func (f *createFlags) options(name string) (sandbox.Options, error) {
	mem, err := sandbox.ParseMemory(f.memory)
	if err != nil {
		return sandbox.Options{}, err
	}
	o := sandbox.Options{
		Name: name, Image: f.image, CPUs: f.cpus, MemoryBytes: mem, Pids: f.pids,
		Network: f.network, Env: f.env, Workdir: f.workdir,
	}
	cwd, _ := os.Getwd()
	for _, s := range f.volumes {
		v, err := sandbox.ParseVolume(s, cwd)
		if err != nil {
			return o, err
		}
		o.Volumes = append(o.Volumes, v)
	}
	return o, nil
}

type execFlags struct {
	env            []string
	workdir        string
	tty, noTTY, bg bool
}

func (f *execFlags) register(c *cobra.Command, withDetach bool) {
	fl := c.Flags()
	fl.StringArrayVarP(&f.env, "env", "e", nil, "environment KEY=VALUE")
	fl.StringVarP(&f.workdir, "workdir", "w", "", "working directory")
	fl.BoolVarP(&f.tty, "tty", "t", false, "force a PTY (default: auto-detect)")
	fl.BoolVarP(&f.noTTY, "no-tty", "T", false, "never allocate a PTY")
	if withDetach {
		fl.BoolVarP(&f.bg, "detach", "d", false, "run in background; output goes to `gsb logs`")
	}
}

func (f *execFlags) options(args []string) sandbox.ExecOptions {
	tty := (f.tty || sandbox.IsTerminal()) && !f.noTTY
	return sandbox.ExecOptions{
		Args: args, Env: f.env, Workdir: f.workdir, TTY: tty, Detach: f.bg,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}
}

// --- lifecycle ---

func createCmd(ctx context.Context) *cobra.Command {
	var f createFlags
	c := &cobra.Command{
		Use:     "create NAME --image IMAGE",
		Short:   "Create and start a long-lived sandbox",
		Example: "  gsb create dev --image golang:1.23 --cpus 2 -m 1g -v .:/src:rw -w /src",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if f.image == "" {
				return fmt.Errorf("--image is required")
			}
			o, err := f.options(args[0])
			if err != nil {
				return err
			}
			if _, err := manager().Create(ctx, o); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
	f.register(c, true)
	return c
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use: "start NAME...", Short: "Start stopped sandboxes (filesystem starts fresh)",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return each(args, manager().Start)
		},
	}
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use: "stop NAME...", Short: "Stop sandboxes (keeps their definition)",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return each(args, manager().Stop)
		},
	}
}

func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm NAME...", Aliases: []string{"delete"}, Short: "Stop and remove sandboxes",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return each(args, manager().Remove)
		},
	}
}

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Aliases: []string{"list", "ps"}, Short: "List sandboxes",
		RunE: func(*cobra.Command, []string) error {
			list, err := manager().List()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tIMAGE\tCPUS\tMEMORY\tNET\tCREATED")
			for _, s := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.State, s.Image,
					orDash(s.CPUs > 0, fmt.Sprint(s.CPUs)), orDash(s.MemoryBytes > 0, fmt.Sprintf("%dM", s.MemoryBytes>>20)),
					s.Network, s.CreatedAt.Format(time.DateTime))
			}
			return w.Flush()
		},
	}
}

// --- running things ---

func execCmd() *cobra.Command {
	var f execFlags
	c := &cobra.Command{
		Use:     "exec NAME [-- COMMAND [ARGS...]]",
		Short:   "Run a command in a sandbox (default: interactive shell)",
		Example: "  gsb exec dev\n  gsb exec dev -- go test ./...\n  gsb exec -d dev -- go run ./server",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return exitWith(manager().Exec(args[0], f.options(args[1:])))
		},
	}
	f.register(c, true)
	return c
}

func runCmd(ctx context.Context) *cobra.Command {
	var cf createFlags
	var ef execFlags
	var name string
	c := &cobra.Command{
		Use:     "run IMAGE [-- COMMAND [ARGS...]]",
		Short:   "Run a command in a throwaway sandbox (create, exec, rm)",
		Example: "  gsb run golang:1.23 -- go version\n  gsb run -v .:/src -w /src golang:1.23 -- go test ./...",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cf.image = args[0]
			o, err := cf.options(name)
			if err != nil {
				return err
			}
			return exitWith(manager().Run(ctx, o, ef.options(args[1:])))
		},
	}
	cf.register(c, false)
	// -e/-w are shared with create flags; exec-only flags:
	c.Flags().BoolVarP(&ef.tty, "tty", "t", false, "force a PTY (default: auto-detect)")
	c.Flags().BoolVarP(&ef.noTTY, "no-tty", "T", false, "never allocate a PTY")
	c.Flags().StringVar(&name, "name", "", "sandbox name (default: random)")
	return c
}

func logsCmd(ctx context.Context) *cobra.Command {
	var follow bool
	c := &cobra.Command{
		Use: "logs NAME", Short: "Show output of the init process and detached execs",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return manager().Logs(ctx, args[0], follow, os.Stdout)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	return c
}

func cpCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "cp SRC DST",
		Short:   "Copy files between host and sandbox (NAME:PATH on one side)",
		Example: "  gsb cp ./main.go dev:/src\n  gsb cp dev:/src/bin ./out",
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			m := manager()
			if name, p, ok := splitRemote(args[1]); ok {
				return m.CopyIn(name, args[0], p)
			}
			if name, p, ok := splitRemote(args[0]); ok {
				if err := os.MkdirAll(args[1], 0o755); err != nil {
					return err
				}
				return m.CopyOut(name, p, args[1])
			}
			return fmt.Errorf("one of SRC or DST must be NAME:PATH")
		},
	}
}

// --- images ---

func pullCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use: "pull IMAGE", Short: "Pull and unpack an image into the cache",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			m := manager()
			if err := os.MkdirAll(m.Images.Dir, 0o700); err != nil {
				return err
			}
			img, err := m.Images.Pull(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s %s\n", img.Ref, img.Digest)
			return nil
		},
	}
}

func imagesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "images", Short: "List cached images",
		RunE: func(*cobra.Command, []string) error {
			list, err := manager().Images.List()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "IMAGE\tDIGEST")
			for _, i := range list {
				fmt.Fprintf(w, "%s\t%s\n", i.Ref, image.Short(i.Digest))
			}
			return w.Flush()
		},
	}
}

// --- project mode (Sandboxfile) ---

func upCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "up [NAME...]",
		Short: "Create or start the Sandboxfile's sandboxes; recreate changed ones",
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := project.Find()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				args = p.Names()
			}
			m := manager()
			return each(args, func(name string) error {
				o, err := p.Options(name)
				if err != nil {
					return err
				}
				if sb, err := m.Store.Load(name); err == nil {
					if sb.Fingerprint == o.Fingerprint() {
						return m.Start(name)
					}
					// Like `compose up`: a changed definition means a new
					// sandbox. Nothing is lost that `stop`/`start` would keep,
					// because the sandbox filesystem is ephemeral anyway.
					fmt.Fprintf(os.Stderr, "gsb: %s: Sandboxfile entry changed, recreating\n", name)
					if err := m.Remove(name); err != nil {
						return err
					}
				}
				_, err = m.Create(ctx, o)
				return err
			})
		},
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use: "down [NAME...]", Short: "Remove the Sandboxfile's sandboxes",
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := project.Find()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				args = p.Names()
			}
			m := manager()
			return each(args, func(n string) error {
				if !m.Store.Exists(n) {
					return nil
				}
				return m.Remove(n)
			})
		},
	}
}

func scriptCmd() *cobra.Command {
	var f execFlags
	c := &cobra.Command{
		Use:     "script SANDBOX SCRIPT [-- ARGS...]",
		Short:   "Run a script defined in the Sandboxfile (ARGS are its \"$@\")",
		Example: "  gsb script dev test\n  gsb script dev test -- -run TestFoo",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := project.Find()
			if err != nil {
				return err
			}
			def, ok := p.Sandboxes[args[0]]
			if !ok {
				return fmt.Errorf("sandbox %q not in Sandboxfile", args[0])
			}
			body, ok := def.Scripts[args[1]]
			if !ok {
				return fmt.Errorf("script %q not defined for %s", args[1], args[0])
			}
			// Extra args become the script's positional parameters; the script
			// decides where they go ("$@", "$1", ...). Appending them to the
			// body would send them only to the last command of `a && b`.
			if len(args) > 2 && !usesArgs.MatchString(body) {
				return fmt.Errorf("script %q takes no arguments; reference them with \"$@\" in the Sandboxfile", args[1])
			}
			cmd := append([]string{"/bin/sh", "-c", body, "gsb-" + args[1]}, args[2:]...)
			return exitWith(manager().Exec(args[0], f.options(cmd)))
		},
	}
	f.register(c, false)
	return c
}

// --- helpers ---

// usesArgs matches shell references to positional parameters.
var usesArgs = regexp.MustCompile(`\$\{?[@*1-9]`)

func each(names []string, fn func(string) error) error {
	var failed []string
	for _, n := range names {
		if err := fn(n); err != nil {
			fmt.Fprintf(os.Stderr, "gsb: %s: %v\n", n, err)
			failed = append(failed, n)
			continue
		}
		fmt.Println(n)
	}
	if len(failed) > 0 {
		return exitErr(1)
	}
	return nil
}

// splitRemote parses NAME:PATH. A leading "." or "/" means a host path.
func splitRemote(s string) (name, path string, ok bool) {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") {
		return "", "", false
	}
	name, path, ok = strings.Cut(s, ":")
	return name, path, ok && name != "" && path != ""
}

func orDash(ok bool, s string) string {
	if ok {
		return s
	}
	return "-"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
