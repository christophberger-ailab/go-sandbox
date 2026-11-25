package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/christophberger/go-sandbox/internal/ocispec"
)

// ParseMemory parses sizes like "512m", "2g", "1048576".
func ParseMemory(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult = 1 << 10
	case strings.HasSuffix(s, "m"):
		mult = 1 << 20
	case strings.HasSuffix(s, "g"):
		mult = 1 << 30
	}
	n, err := strconv.ParseFloat(strings.TrimRight(s, "kmgb"), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// ParseVolume parses "host:guest[:ro|rw]". Relative host paths are resolved
// against base. Volumes are read-only unless ":rw" is given.
func ParseVolume(s, base string) (ocispec.Volume, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return ocispec.Volume{}, fmt.Errorf("invalid volume %q (want host:guest[:ro|rw])", s)
	}
	v := ocispec.Volume{Host: parts[0], Guest: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "rw":
			v.RW = true
		case "ro":
		default:
			return v, fmt.Errorf("invalid volume mode %q", parts[2])
		}
	}
	if !filepath.IsAbs(v.Host) {
		v.Host = filepath.Join(base, v.Host)
	}
	if !filepath.IsAbs(v.Guest) {
		return v, fmt.Errorf("guest path must be absolute: %q", v.Guest)
	}
	if _, err := os.Stat(v.Host); err != nil {
		return v, fmt.Errorf("volume source: %w", err)
	}
	return v, nil
}
