//go:build windows

package main

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// The process page used to start Windows PowerShell twice on every slow pass:
// Get-Process supplied CPU/memory and Get-Counter supplied process I/O rates.
// These kernel32 calls collect the same cumulative values in-process. Besides
// avoiding interpreter startup, cumulative counters let us calculate CPU and
// I/O over exactly the same wall interval.
var (
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procGetProcessTimes          = kernel32.NewProc("GetProcessTimes")
	procGetProcessIoCounters     = kernel32.NewProc("GetProcessIoCounters")
	procK32GetProcessMemoryInfo  = kernel32.NewProc("K32GetProcessMemoryInfo")
)

const (
	th32csSnapProcess               = 0x00000002
	processQueryLimitedInformation  = 0x1000
	windowsFiletimeTicksPerSecond   = 10_000_000
	windowsMaxProcessExecutablePath = 260
)

type processEntry32 struct {
	Size              uint32
	Usage             uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExecutableFile    [windowsMaxProcessExecutablePath]uint16
}

type processMemoryCounters struct {
	Size                       uint32
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

type processIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type windowsNativeProcessRow struct {
	pid         int
	name        string
	created     uint64
	cpuSeconds  float64
	workingSet  uint64
	diskRead    uint64
	diskWrite   uint64
	cpuOK       bool
	memoryOK    bool
	diskOK      bool
	cpuError    string
	memoryError string
	diskError   string
}

// collectProcesses builds process and app metrics without creating child
// processes. Inaccessible and short-lived PIDs remain visible by name while
// their protected fields degrade independently.
func (c *systemCollector) collectProcesses(ctx context.Context) ProcessSet {
	rows, err := readWindowsProcesses(ctx)
	if err != nil {
		return unavailableProcessSet("native Windows process enumeration failed: " + err.Error())
	}
	now := time.Now()
	gpuMem := c.cachedNVIDIAProcessGPUMemory(ctx)
	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}

	c.mu.Lock()
	previous := c.prevProc
	c.mu.Unlock()

	current := make(map[int]procSample, len(rows))
	raw := make([]ProcessMetric, 0, len(rows))
	for _, row := range rows {
		pm := ProcessMetric{PID: row.pid, Name: row.name}
		if row.memoryOK {
			pm.Memory = availableNumber(float64(row.workingSet), "B")
		} else {
			pm.Memory = unavailableNumber("B", row.memoryError)
		}

		prev, hadPrev := previous[row.pid]
		sameProcess := sameWindowsProcess(prev, hadPrev, row)
		if sameProcess {
			pm.CPU = processCPUPercent(row.cpuSeconds, prev.cpuSeconds, now.Sub(prev.ts).Seconds(), numCPU)
		}
		if !pm.CPU.Available {
			reason := "process sampler is warming up"
			if !row.cpuOK {
				reason = row.cpuError
			}
			pm.CPU = unavailableNumber("%", reason)
		}

		pm.DiskRead, pm.DiskWrite = windowsProcessIORates(row, prev, sameProcess, now)
		if bytes, onGPU := gpuMem[row.pid]; onGPU {
			pm.GPUMemory = availableNumber(float64(bytes), "B")
		} else {
			pm.GPUMemory = unavailableNumber("B", "not a CUDA/compute process")
		}

		if row.cpuOK {
			current[row.pid] = procSample{
				cpuSeconds: row.cpuSeconds,
				ts:         now,
				created:    row.created,
				diskRead:   row.diskRead,
				diskWrite:  row.diskWrite,
				diskAvail:  row.diskOK,
			}
		}
		raw = append(raw, pm)
	}

	c.mu.Lock()
	c.prevProc = current
	c.mu.Unlock()
	return buildProcessSet(raw, len(raw))
}

func sameWindowsProcess(prev procSample, hadPrev bool, row windowsNativeProcessRow) bool {
	return hadPrev && row.cpuOK && row.created != 0 && prev.created == row.created
}

func windowsProcessIORates(row windowsNativeProcessRow, prev procSample, sameProcess bool, now time.Time) (NumberMetric, NumberMetric) {
	if !row.diskOK {
		reason := row.diskError
		if reason == "" {
			reason = "process I/O counters unavailable"
		}
		return unavailableNumber("B/s", reason), unavailableNumber("B/s", reason)
	}
	if !sameProcess || !prev.diskAvail {
		return unavailableNumber("B/s", "process I/O sampler is warming up"), unavailableNumber("B/s", "process I/O sampler is warming up")
	}
	elapsed := now.Sub(prev.ts).Seconds()
	return counterRate(row.diskRead, prev.diskRead, elapsed), counterRate(row.diskWrite, prev.diskWrite, elapsed)
}

func counterRate(current, previous uint64, elapsed float64) NumberMetric {
	if elapsed <= 0 || current < previous {
		return unavailableNumber("B/s", "process I/O counter reset")
	}
	return availableNumber(float64(current-previous)/elapsed, "B/s")
}

func readWindowsProcesses(ctx context.Context) ([]windowsNativeProcessRow, error) {
	snapshot, _, callErr := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snapshot == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %v", callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(snapshot))

	entry := processEntry32{Size: uint32(unsafe.Sizeof(processEntry32{}))}
	ok, _, callErr := procProcess32FirstW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		return nil, fmt.Errorf("Process32FirstW: %v", callErr)
	}

	rows := make([]windowsNativeProcessRow, 0, 256)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid := int(entry.ProcessID)
		if pid > 0 {
			name := syscall.UTF16ToString(entry.ExecutableFile[:])
			if name == "" {
				name = fmt.Sprintf("pid %d", pid)
			}
			rows = append(rows, readWindowsProcess(pid, name))
		}

		entry.Size = uint32(unsafe.Sizeof(entry))
		ok, _, _ = procProcess32NextW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
		if ok == 0 {
			break
		}
	}
	return rows, nil
}

func readWindowsProcess(pid int, name string) windowsNativeProcessRow {
	row := windowsNativeProcessRow{pid: pid, name: name}
	handle, _, callErr := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		reason := fmt.Sprintf("OpenProcess failed: %v", callErr)
		row.cpuError, row.memoryError, row.diskError = reason, reason, reason
		return row
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	var created, exited, kernelTime, userTime syscall.Filetime
	if ok, _, err := procGetProcessTimes.Call(
		handle,
		uintptr(unsafe.Pointer(&created)),
		uintptr(unsafe.Pointer(&exited)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	); ok != 0 {
		row.created = filetimeTicks(created)
		row.cpuSeconds = float64(filetimeTicks(kernelTime)+filetimeTicks(userTime)) / windowsFiletimeTicksPerSecond
		row.cpuOK = true
	} else {
		row.cpuError = fmt.Sprintf("GetProcessTimes failed: %v", err)
	}

	memory := processMemoryCounters{Size: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	if ok, _, err := procK32GetProcessMemoryInfo.Call(handle, uintptr(unsafe.Pointer(&memory)), uintptr(memory.Size)); ok != 0 {
		row.workingSet = uint64(memory.WorkingSetSize)
		row.memoryOK = true
	} else {
		row.memoryError = fmt.Sprintf("K32GetProcessMemoryInfo failed: %v", err)
	}

	var io processIOCounters
	if ok, _, err := procGetProcessIoCounters.Call(handle, uintptr(unsafe.Pointer(&io))); ok != 0 {
		row.diskRead = io.ReadTransferCount
		row.diskWrite = io.WriteTransferCount
		row.diskOK = true
	} else {
		row.diskError = fmt.Sprintf("GetProcessIoCounters failed: %v", err)
	}
	return row
}
