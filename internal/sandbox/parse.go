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
	num := strings.ToLower(strings.TrimSpace(s))
	// Accept "512m", "512mb", "512mib" (all binary units), and plain bytes.
	num = strings.TrimSuffix(num, "b")
	num = strings.TrimSuffix(num, "i")
	mult := int64(1)
	if n := len(num); n > 0 {
		switch num[n-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		}
		if mult > 1 {
			num = num[:n-1]
		}
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// ParseVolume parses "host:guest[:ro|rw]". Relative host paths are resolved
// against base. Volumes are read-only unless ":rw" is given. It splits from
// the right, so host paths may contain ":" (guest paths may not).
func ParseVolume(s, base string) (ocispec.Volume, error) {
	var v ocispec.Volume
	rest := s
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		switch rest[i+1:] {
		case "rw":
			v.RW = true
			rest = rest[:i]
		case "ro":
			rest = rest[:i]
		}
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return v, fmt.Errorf("invalid volume %q (want host:guest[:ro|rw])", s)
	}
	v.Host, v.Guest = rest[:i], rest[i+1:]
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
