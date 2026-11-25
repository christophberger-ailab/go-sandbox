package sandbox

import (
	"os"
	"testing"
)

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "512m": 512 << 20, "2g": 2 << 30, "1.5G": 3 << 29, "4096": 4096} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestParseVolume(t *testing.T) {
	v, err := ParseVolume(".:/src", "/tmp")
	if err != nil || v.Host != "/tmp" || v.Guest != "/src" || v.RW {
		t.Errorf("default should be read-only bind of /tmp: %+v %v", v, err)
	}
	if v, _ := ParseVolume("/tmp:/src:rw", "/"); !v.RW {
		t.Error(":rw not honored")
	}
	if _, err := ParseVolume("/tmp:relative", "/"); err == nil {
		t.Error("relative guest path accepted")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"PATH=/bin", "A=1"}, []string{"A=2", "B=3"})
	want := []string{"PATH=/bin", "A=2", "B=3"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseMemoryUnits(t *testing.T) {
	for in, want := range map[string]int64{"512mb": 512 << 20, "512MiB": 512 << 20, "1gb": 1 << 30, "64k": 64 << 10, "100b": 100} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"m", "12x", "-1g", "1kmg"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) accepted", bad)
		}
	}
}

func TestParseVolumeColonInHostPath(t *testing.T) {
	dir := t.TempDir() + "/a:b"
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := ParseVolume(dir+":/data:rw", "/")
	if err != nil || v.Host != dir || v.Guest != "/data" || !v.RW {
		t.Fatalf("got %+v, %v", v, err)
	}
	if v, err := ParseVolume(dir+":/data", "/"); err != nil || v.Host != dir || v.RW {
		t.Fatalf("got %+v, %v", v, err)
	}
	for _, bad := range []string{"/tmp", "/tmp:", ":/data", "/tmp:/data:xx"} {
		if _, err := ParseVolume(bad, "/"); err == nil {
			t.Errorf("ParseVolume(%q) accepted", bad)
		}
	}
}
