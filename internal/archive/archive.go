// Package archive extracts and creates tar streams. Extraction is confined to
// a root directory: every path is resolved with securejoin so that "../" and
// symlinks planted by an earlier entry cannot escape the root.
package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"
	"golang.org/x/sys/unix"
)

// Untar extracts r into root. If preserveOwner is set (needs root), file
// ownership and device nodes are restored; otherwise they are skipped.
func Untar(r io.Reader, root string, preserveOwner bool) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := extractEntry(tr, hdr, root, preserveOwner); err != nil {
			return fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
	}
}

func extractEntry(tr *tar.Reader, hdr *tar.Header, root string, preserveOwner bool) error {
	// Resolve the parent safely, but never follow the final component:
	// a symlink entry must be created as a link, not resolved.
	parent, err := securejoin.SecureJoin(root, filepath.Dir(filepath.Clean("/"+hdr.Name)))
	if err != nil {
		return err
	}
	base := filepath.Base(hdr.Name)
	if base == "/" || base == "." {
		return nil // the root itself
	}
	target := filepath.Join(parent, base)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	mode := hdr.FileInfo().Mode()

	switch hdr.Typeflag {
	case tar.TypeDir:
		if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			_ = os.Remove(target)
		}
		if err := os.Mkdir(target, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	case tar.TypeReg:
		_ = os.Remove(target)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		_ = os.RemoveAll(target)
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		src, err := securejoin.SecureJoin(root, hdr.Linkname)
		if err != nil {
			return err
		}
		_ = os.RemoveAll(target)
		return os.Link(src, target)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if !preserveOwner {
			return nil
		}
		_ = os.RemoveAll(target)
		devMode := uint32(mode.Perm())
		switch hdr.Typeflag {
		case tar.TypeChar:
			devMode |= unix.S_IFCHR
		case tar.TypeBlock:
			devMode |= unix.S_IFBLK
		default:
			devMode |= unix.S_IFIFO
		}
		dev := int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))
		if err := unix.Mknod(target, devMode, dev); err != nil {
			return err
		}
	default:
		return nil // ignore exotic types (xattrs headers etc.)
	}

	if preserveOwner {
		if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
			return err
		}
	}
	if hdr.Typeflag != tar.TypeSymlink {
		// chmod after chown, because chown clears setuid/setgid bits.
		return os.Chmod(target, mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	}
	return nil
}

// Tar writes src (a file or directory) to w. Entries are named relative to
// src's parent, so "tar -x -C dest" recreates filepath.Base(src) under dest.
func Tar(w io.Writer, src string) error {
	tw := tar.NewWriter(w)
	base := filepath.Dir(filepath.Clean(src))
	err := filepath.Walk(src, func(path string, fi fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
