package bench

import (
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

// cpuModel reads the processor name from the registry (reg.exe ships with Windows).
func cpuModel() string {
	out, err := exec.Command("reg", "query", `HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "/v", "ProcessorNameString").Output()
	if err != nil {
		return ""
	}
	if _, v, ok := strings.Cut(string(out), "REG_SZ"); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func memKB() int64 {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	ok, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&m)))
	if ok == 0 {
		return 0
	}
	return int64(m.totalPhys >> 10)
}

func osName() string { return "Windows" }
