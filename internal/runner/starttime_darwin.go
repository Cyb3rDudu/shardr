//go:build darwin && cgo

package runner

/*
#include <libproc.h>
#include <sys/proc_info.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// darwinProcStart extracts the process start time with MICROSECOND
// resolution from the kernel's process structure via
// proc_pidinfo(PROC_PIDTBSDINFO) — the source dudu's round-3 order
// names ("z. B. proc_pidinfo/libproc"). The ps lstart fallback
// (second-granular) deliberately no longer participates: two pids
// spawned in the same wall-clock second must not share a token.
// Returns "" when the structure is unreadable (callers fail loud).
func darwinProcStart(pid int) string {
	var pbi C.struct_proc_bsdinfo
	r := C.proc_pidinfo(C.int(pid), C.PROC_PIDTBSDINFO, 0, unsafe.Pointer(&pbi), C.int(unsafe.Sizeof(pbi)))
	if r <= 0 {
		return ""
	}
	return fmt.Sprintf("%d.%06d", int64(pbi.pbi_start_tvsec), int64(pbi.pbi_start_tvusec))
}
