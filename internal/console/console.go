// Package console implements the runc/runsc "console socket" protocol:
// gsb listens on a Unix socket, runsc creates a PTY inside the sandbox and
// sends the master fd over the socket (SCM_RIGHTS). gsb then bridges the
// user's terminal to that master. This is what makes `gsb exec` interactive.
package console

import (
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Socket is a listening console socket.
type Socket struct {
	Path string
	ln   *net.UnixListener
	dir  string
}

func NewSocket() (*Socket, error) {
	dir, err := os.MkdirTemp("", "gsb-console-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "console.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Socket{Path: path, ln: ln, dir: dir}, nil
}

func (s *Socket) Close() { s.ln.Close(); os.RemoveAll(s.dir) }

// Receive waits for runsc to connect and returns the PTY master.
func (s *Socket) Receive(timeout time.Duration) (*os.File, error) {
	s.ln.SetDeadline(time.Now().Add(timeout))
	conn, err := s.ln.AcceptUnix()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	buf := make([]byte, 256)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return nil, errors.New("console socket: no control message")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return nil, errors.New("console socket: no fd received")
	}
	return os.NewFile(uintptr(fds[0]), "pty-master"), nil
}

// Attach bridges stdin/stdout to the PTY master: raw mode, window-size
// propagation, and copying in both directions. Call the returned function
// after the process exits; it drains output and restores the terminal.
func Attach(master *os.File) (done func()) {
	stdin := int(os.Stdin.Fd())
	var restore func()
	if term.IsTerminal(stdin) {
		if st, err := term.MakeRaw(stdin); err == nil {
			restore = func() { term.Restore(stdin, st) }
		}
	}

	resize := func() {
		if ws, err := unix.IoctlGetWinsize(stdin, unix.TIOCGWINSZ); err == nil {
			unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws)
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, unix.SIGWINCH)
	go func() {
		for range winch {
			resize()
		}
	}()

	go io.Copy(master, os.Stdin)
	outDone := make(chan struct{})
	go func() { io.Copy(os.Stdout, master); close(outDone) }()

	return func() {
		// The master returns EIO once the slave side closes; don't hang if
		// a background process inside still holds it open.
		select {
		case <-outDone:
		case <-time.After(500 * time.Millisecond):
		}
		signal.Stop(winch)
		close(winch)
		master.Close()
		if restore != nil {
			restore()
		}
	}
}
