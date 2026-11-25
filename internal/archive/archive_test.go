package archive

import (
	"archive/tar"
	"bytes"
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
