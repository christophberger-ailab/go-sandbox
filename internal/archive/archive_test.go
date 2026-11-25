package archive

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A malicious layer plants a symlink to the outside, then writes through it.
// Both writes must land inside root.
func TestUntarStaysInsideRoot(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside})
	write := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	write("escape/pwned", "x")
	write("../../pwned2", "y")
	tw.Close()

	if err := Untar(&buf, root, false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("extraction escaped root: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(root, "pwned2")); err != nil {
		t.Fatalf("expected ../../pwned2 to be clamped into root: %v", err)
	}
}

func TestTarRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "main.go"), []byte("package main"), 0o644)
	os.Symlink("sub/main.go", filepath.Join(src, "link"))

	var buf bytes.Buffer
	if err := Tar(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Untar(&buf, dst, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "proj", "link"))
	if err != nil || string(b) != "package main" {
		t.Fatalf("round trip: %q, %v", b, err)
	}
}

func tarOf(t *testing.T, hdrs ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Linkname)) // body carried in Linkname for brevity
			body := h.Linkname
			h.Linkname = ""
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			tw.Write([]byte(body))
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return &buf
}

// Regression test: a symlink entry named "x/.." used to replace the
// directory above root with a symlink, so later entries landed outside.
func TestUntarDotDotSymlinkEntry(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	os.Mkdir(root, 0o755)
	sibling := filepath.Join(parent, "keep")
	os.WriteFile(sibling, []byte("x"), 0o644)
	elsewhere := t.TempDir()

	buf := tarOf(t,
		&tar.Header{Name: "x/..", Typeflag: tar.TypeSymlink, Linkname: elsewhere},
		&tar.Header{Name: "../pwned", Typeflag: tar.TypeReg, Mode: 0o644, Linkname: "y"},
	)
	_ = Untar(buf, root, false)

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("file next to root was deleted: %v", err)
	}
	if fi, err := os.Lstat(parent); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		t.Fatalf("directory above root was replaced: %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("write escaped root: %v", entries)
	}
}

// Regression test: a directory entry "x/.." used to chmod the directory above root.
func TestUntarDotDotDirEntry(t *testing.T) {
	parent := t.TempDir()
	os.Chmod(parent, 0o700)
	root := filepath.Join(parent, "root")
	os.Mkdir(root, 0o755)

	buf := tarOf(t, &tar.Header{Name: "x/..", Typeflag: tar.TypeDir, Mode: 0o777})
	if err := Untar(buf, root, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(parent); fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory above root was chmodded to %v", fi.Mode().Perm())
	}
}

// Untrusted archives must not produce setuid/setgid files.
func TestUntarDropsSetuidWithoutPreserveOwner(t *testing.T) {
	root := t.TempDir()
	buf := tarOf(t, &tar.Header{Name: "sh", Typeflag: tar.TypeReg, Mode: 0o4755 | 0o2000, Linkname: "#!"})
	if err := Untar(buf, root, false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		t.Fatalf("setuid/setgid kept: %v", fi.Mode())
	}
}
