//go:build darwin

package runner

import (
	"encoding/binary"
	"strings"
	"syscall"
)

// darwinBootTime reads kern.boottime (seconds since epoch) — darwin-only
// sysctl; linux derives the boot time from /proc/stat (registry.go).
func darwinBootTime() string {
	b, err := syscall.Sysctl("kern.boottime")
	if err != nil || len(b) < 8 {
		return "?"
	}
	var sec int64
	if err := binary.Read(strings.NewReader(b[:8]), binary.LittleEndian, &sec); err != nil {
		return "?"
	}
	return itoa(sec)
}

func itoa(v int64) string {
	// strconv without dragging it into both files
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
