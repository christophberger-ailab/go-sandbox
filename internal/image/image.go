// Package image pulls OCI images and unpacks them into a read-only rootfs
// cache, keyed by manifest digest. Sandboxes never write to this cache:
// runsc's rootfs overlay (--overlay2) keeps every write inside the sandbox.
package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/christophberger/go-sandbox/internal/archive"
)

// Image is an unpacked image in the cache.
type Image struct {
	Ref    string        `json:"ref"`
	Digest string        `json:"digest"`
	Rootfs string        `json:"rootfs"`
	Config v1.ConfigFile `json:"config"`
}

// Store is the on-disk image cache under Dir.
//
//	Dir/index.json                 ref -> digest
//	Dir/<digest-hex>/rootfs/       unpacked, flattened layers
//	Dir/<digest-hex>/image.json    Image metadata incl. OCI config
type Store struct {
	Dir string
	mu  sync.Mutex
}

// Get returns a cached image, pulling it if the ref is unknown.
func (s *Store) Get(ctx context.Context, ref string) (*Image, error) {
	if img, err := s.lookup(ref); err == nil {
		return img, nil
	}
	return s.Pull(ctx, ref)
}

// Pull always contacts the registry and unpacks the image if its digest is new.
func (s *Store) Pull(ctx context.Context, ref string) (*Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	remoteImg, err := remote.Image(r,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	digest, err := remoteImg.Digest()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Dir, digest.Hex)

	img, err := readMeta(dir)
	if err != nil { // not cached yet: unpack into a temp dir, then rename
		cfg, err := remoteImg.ConfigFile()
		if err != nil {
			return nil, err
		}
		tmp, err := os.MkdirTemp(s.Dir, ".unpack-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		rootfs := filepath.Join(tmp, "rootfs")
		if err := os.Mkdir(rootfs, 0o755); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "unpacking %s (%s)...\n", ref, digest.Hex[:12])
		rc := mutate.Extract(remoteImg) // flattened, whiteouts applied
		err = archive.Untar(rc, rootfs, os.Geteuid() == 0)
		rc.Close()
		if err != nil {
			return nil, err
		}
		img = &Image{Ref: ref, Digest: digest.String(), Rootfs: filepath.Join(dir, "rootfs"), Config: *cfg}
		if err := writeJSON(filepath.Join(tmp, "image.json"), img); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, dir); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	return img, s.setIndex(ref, digest.Hex)
}

// List returns all cached images.
func (s *Store) List() ([]Image, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, hex := range idx {
		if img, err := readMeta(filepath.Join(s.Dir, hex)); err == nil {
			out = append(out, *img)
		}
	}
	return out, nil
}

func (s *Store) lookup(ref string) (*Image, error) {
	idx, err := s.index()
	if err != nil {
		return nil, err
	}
	hex, ok := idx[ref]
	if !ok {
		return nil, os.ErrNotExist
	}
	return readMeta(filepath.Join(s.Dir, hex))
}

func (s *Store) index() (map[string]string, error) {
	idx := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.Dir, "index.json"))
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (s *Store) setIndex(ref, hex string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.index()
	if err != nil {
		return err
	}
	idx[ref] = hex
	return writeJSON(filepath.Join(s.Dir, "index.json"), idx)
}

func readMeta(dir string) (*Image, error) {
	b, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(b, &img); err != nil {
		return nil, err
	}
	img.Rootfs = filepath.Join(dir, "rootfs") // dir may have been renamed from tmp
	return &img, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Short returns a human-friendly digest prefix.
func Short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}
