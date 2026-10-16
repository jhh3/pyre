//go:build windows

package sysstats

import (
	"errors"
	"hash/fnv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 readers. Everything is a direct kernel32/ntdll call: no WMI, no
// PowerShell, no subprocess; each sample is bounded by the process count.

var (
	modkernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx    = modkernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes          = modkernel32.NewProc("GetSystemTimes")
	procK32GetProcessMemoryInfo = modkernel32.NewProc("K32GetProcessMemoryInfo")
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func ftTicks(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// NativeMemory returns physical memory total/available bytes.
func NativeMemory() (total, avail uint64, err error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0, e
	}
	return m.TotalPhys, m.AvailPhys, nil
}

func winRaw() (WinRaw, error) {
	var r WinRaw
	var err error
	r.MemTotal, r.MemAvail, err = NativeMemory()
	if err != nil {
		return r, err
	}
	var idle, kernel, user windows.Filetime
	if ok, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok != 0 {
		r.Idle, r.Kernel, r.User = ftTicks(idle), ftTicks(kernel), ftTicks(user)
	}
	if total, inUse, ok := pagefiles(); ok {
		page := uint64(windows.Getpagesize())
		r.PageTotal, r.PageInUse, r.HavePagefile = total*page, inUse*page, true
	}
	return r, nil
}

// pagefiles reads SystemPageFileInformation (sizes in pages). The buffer
// grows to at most 64 KiB (one entry per pagefile, a few dozen bytes each).
func pagefiles() (total, inUse uint64, ok bool) {
	for size := uint32(4 << 10); size <= 64<<10; size *= 2 {
		buf := make([]byte, size)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemPageFileInformation, unsafe.Pointer(&buf[0]), size, &ret)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			continue
		}
		if err != nil {
			return 0, 0, false
		}
		if ret > size {
			ret = size
		}
		t, u := parsePageFiles(buf[:ret])
		return t, u, true // no pagefile: ret 0, swap 0
	}
	return 0, 0, false
}

func statfs(path string) (FSInfo, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FSInfo{}, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return FSInfo{}, err
	}
	fi := FSInfo{Total: total, Avail: avail}
	// Dedupe by volume mount point ("C:\", or a mounted folder).
	vol := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &vol[0], uint32(len(vol))); err == nil {
		h := fnv.New64a()
		_, _ = h.Write([]byte(strings.ToLower(windows.UTF16ToString(vol))))
		fi.Dev = h.Sum64()
	}
	return fi, nil
}

// procInfo reads CPU times, creation time and working set of one process.
// Processes we may not open (other users', protected) keep zero usage.
func procInfo(pid uint32, p *Proc) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		h, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			return
		}
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		p.CPU = filetimeDuration(ftTicks(kernel) + ftTicks(user))
		p.Start = time.Unix(0, created.Nanoseconds())
	}
	var mc processMemoryCounters
	mc.Cb = uint32(unsafe.Sizeof(mc))
	if r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.Cb)); r != 0 {
		p.RSSBytes = int64(mc.WorkingSetSize)
	}
}

// NativeProcTable returns every process (Toolhelp snapshot) with parent
// PID, working set (RSS equivalent), cumulative CPU and start time. Unlike
// the Unix tables, CPU does not include reaped children: Windows keeps no
// per-process children total, so a tree's CPU delta misses tool commands
// that exited between two samples.
func NativeProcTable() (map[int]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	m := make(map[int]Proc, 256)
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		p := Proc{PID: int(e.ProcessID), PPID: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
		if e.ProcessID != 0 {
			procInfo(e.ProcessID, &p)
		}
		m[p.PID] = p
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return m, nil
}

// NativeProcess samples one process (no PPID or name).
func NativeProcess(pid int) (Proc, error) {
	if pid <= 0 {
		return Proc{}, errors.New("sysstats: invalid pid")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Proc{}, err
	}
	var code uint32
	alive := windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
	windows.CloseHandle(h)
	if !alive {
		return Proc{}, errors.New("sysstats: process has exited")
	}
	p := Proc{PID: pid}
	procInfo(uint32(pid), &p)
	return p, nil
}

// NativeSelf returns this process's cumulative CPU and working set.
func NativeSelf() (time.Duration, int64, error) {
	h := windows.CurrentProcess()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, 0, err
	}
	cpu := filetimeDuration(ftTicks(kernel) + ftTicks(user))
	var mc processMemoryCounters
	mc.Cb = uint32(unsafe.Sizeof(mc))
	var rss int64
	if r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.Cb)); r != 0 {
		rss = int64(mc.WorkingSetSize)
	}
	return cpu, rss, nil
}
