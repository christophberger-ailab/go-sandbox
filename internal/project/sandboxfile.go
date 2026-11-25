// Package project reads a Sandboxfile: named sandboxes plus scripts, so a
// repo can declare its environment and `gsb up` / `gsb script` just work.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/christophberger/go-sandbox/internal/ocispec"
	"github.com/christophberger/go-sandbox/internal/sandbox"
)

// Def is one sandbox entry in the Sandboxfile.
type Def struct {
	Image   string            `yaml:"image"`
	CPUs    float64           `yaml:"cpus"`
	Memory  string            `yaml:"memory"`
	Pids    int64             `yaml:"pids"`
	Network string            `yaml:"network"`
	Workdir string            `yaml:"workdir"`
	Volumes []string          `yaml:"volumes"`
	Env     map[string]string `yaml:"env"`
	Scripts map[string]string `yaml:"scripts"`
}

type Project struct {
	Dir       string
	Sandboxes map[string]Def `yaml:"sandboxes"`
}

// Find walks up from the current directory looking for a Sandboxfile.
func Find() (*Project, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		for _, n := range []string{"Sandboxfile", "Sandboxfile.yaml"} {
			if p := filepath.Join(dir, n); fileExists(p) {
				return Load(p)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errors.New("no Sandboxfile found in this or any parent directory")
		}
		dir = parent
	}
}

func Load(path string) (*Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := &Project{Dir: filepath.Dir(path)}
	if err := yaml.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Names returns sandbox names in stable order.
func (p *Project) Names() []string {
	names := make([]string, 0, len(p.Sandboxes))
	for n := range p.Sandboxes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Options converts a definition into create options.
func (p *Project) Options(name string) (sandbox.Options, error) {
	d, ok := p.Sandboxes[name]
	if !ok {
		return sandbox.Options{}, fmt.Errorf("sandbox %q not defined in Sandboxfile", name)
	}
	mem, err := sandbox.ParseMemory(d.Memory)
	if err != nil {
		return sandbox.Options{}, err
	}
	var vols []ocispec.Volume
	for _, s := range d.Volumes {
		v, err := sandbox.ParseVolume(s, p.Dir)
		if err != nil {
			return sandbox.Options{}, err
		}
		vols = append(vols, v)
	}
	var env []string
	for k, v := range d.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return sandbox.Options{
		Name: name, Image: d.Image, CPUs: d.CPUs, MemoryBytes: mem, Pids: d.Pids,
		Network: d.Network, Workdir: d.Workdir, Volumes: vols, Env: env,
	}, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
