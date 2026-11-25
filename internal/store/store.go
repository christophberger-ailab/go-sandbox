// Package store keeps one directory per sandbox:
//
//	<root>/sandboxes/<name>/sandbox.json   the Sandbox record
//	<root>/sandboxes/<name>/bundle/        OCI bundle (config.json)
//	<root>/sandboxes/<name>/output.log     init + detached exec output
//
// A flock on <root>/lock serializes mutations across concurrent gsb calls.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/christophberger/go-sandbox/internal/ocispec"
)

// Sandbox is the persisted description of a sandbox. Its live status comes
// from runsc, not from this record.
type Sandbox struct {
	Name        string           `json:"name"`
	ID          string           `json:"id"` // runsc container id
	Image       string           `json:"image"`
	ImageDigest string           `json:"imageDigest"`
	CPUs        float64          `json:"cpus,omitempty"`
	MemoryBytes int64            `json:"memoryBytes,omitempty"`
	Pids        int64            `json:"pids,omitempty"`
	Network     string           `json:"network"`
	Volumes     []ocispec.Volume `json:"volumes,omitempty"`
	Env         []string         `json:"env"`     // image env + user env
	Workdir     string           `json:"workdir"` // default cwd for exec
	UID         uint32           `json:"uid"`
	GID         uint32           `json:"gid"`
	Ephemeral   bool             `json:"ephemeral,omitempty"`
	CreatedAt   time.Time        `json:"createdAt"`
}

type Store struct{ Root string }

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid sandbox name %q (use letters, digits, '.', '_', '-')", name)
	}
	return nil
}

func (s *Store) Dir(name string) string    { return filepath.Join(s.Root, "sandboxes", name) }
func (s *Store) Bundle(name string) string { return filepath.Join(s.Dir(name), "bundle") }
func (s *Store) Log(name string) string    { return filepath.Join(s.Dir(name), "output.log") }

// Lock takes the global gsb lock and returns its release function.
func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

func (s *Store) Exists(name string) bool {
	_, err := os.Stat(filepath.Join(s.Dir(name), "sandbox.json"))
	return err == nil
}

func (s *Store) Save(sb *Sandbox) error {
	if err := os.MkdirAll(s.Bundle(sb.Name), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir(sb.Name), "sandbox.json"), b, 0o600)
}

func (s *Store) Load(name string) (*Sandbox, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir(name), "sandbox.json"))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no such sandbox: %s", name)
	}
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	return &sb, json.Unmarshal(b, &sb)
}

func (s *Store) List() ([]*Sandbox, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "sandboxes"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Sandbox
	for _, e := range entries {
		if sb, err := s.Load(e.Name()); err == nil {
			out = append(out, sb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) Delete(name string) error { return os.RemoveAll(s.Dir(name)) }
