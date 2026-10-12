//go:build windows

package main

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Native reads of the "GPU Engine" counter set and the DXGI adapter list (see
// gpu_engines.go for why). Both go straight to pdh.dll and dxgi.dll: the
// fallback GPU path's Get-Counter call starts PowerShell, which is exactly
// the per-pass cost the 2026-09-10 observer-effect fix removed elsewhere.

var (
	pdhDLL                          = syscall.NewLazyDLL("pdh.dll")
	procPdhOpenQueryW               = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW       = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData         = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArray = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery               = pdhDLL.NewProc("PdhCloseQuery")

	dxgiDLL                = syscall.NewLazyDLL("dxgi.dll")
	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtNoCap100 = 0x00008000
	pdhMoreData    = 0x800007D2
	// PDH_CSTATUS_VALID_DATA and PDH_CSTATUS_NEW_DATA; anything else (a rate
	// with one sample, an instance that just appeared) is skipped.
	pdhCStatusValid = 0
	pdhCStatusNew   = 1

	gpuEngineCounterPath = `\GPU Engine(*)\Utilization Percentage`
	// dxgiAdapterTTL bounds how long the adapter list is trusted; adapters
	// only change on driver installs and eGPU plugs.
	dxgiAdapterTTL = 10 * time.Minute
	// DXGI_ADAPTER_FLAG_SOFTWARE: the Microsoft Basic Render Driver.
	dxgiAdapterFlagSoftware = 2
)

// pdhCounterValueItem mirrors PDH_FMT_COUNTERVALUE_ITEM_W on 64-bit Windows:
// the instance name pointer, then PDH_FMT_COUNTERVALUE (a status DWORD padded
// to the 8-byte union holding the double).
type pdhCounterValueItem struct {
	Name    *uint16
	CStatus uint32
	_       uint32
	Value   float64
}

// gpuEngineSampler owns the persistent PDH query. A rate counter needs two
// collections, so the first pass after start reports warming up and every
// later one covers the interval since the previous pass. Guarded by mu.
type gpuEngineSampler struct {
	mu        sync.Mutex
	query     uintptr
	counter   uintptr
	opened    bool
	openErr   error
	collected bool
	buf       []byte

	adapters   []dxgiAdapterInfo
	adaptersAt time.Time
	adapterErr error
}

func (s *gpuEngineSampler) ensureOpenLocked() error {
	if s.opened {
		return s.openErr
	}
	s.opened = true
	var query uintptr
	if status, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); status != 0 {
		s.openErr = fmt.Errorf("PdhOpenQuery failed: 0x%08X", status)
		return s.openErr
	}
	path, err := syscall.UTF16PtrFromString(gpuEngineCounterPath)
	if err != nil {
		procPdhCloseQuery.Call(query)
		s.openErr = err
		return err
	}
	var counter uintptr
	if status, _, _ := procPdhAddEnglishCounterW.Call(query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter))); status != 0 {
		procPdhCloseQuery.Call(query)
		s.openErr = fmt.Errorf("no GPU Engine performance counters (PdhAddEnglishCounter 0x%08X)", status)
		return s.openErr
	}
	s.query, s.counter = query, counter
	return nil
}

// collect runs one PDH collection and returns every valid instance.
func (s *gpuEngineSampler) collect() ([]gpuEngineReading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	if status, _, _ := procPdhCollectQueryData.Call(s.query); status != 0 {
		return nil, fmt.Errorf("PdhCollectQueryData failed: 0x%08X", status)
	}
	if !s.collected {
		s.collected = true
		return nil, errGPUEnginesWarming
	}
	format := uintptr(pdhFmtDouble | pdhFmtNoCap100)
	for attempt := 0; attempt < 3; attempt++ {
		size := uint32(len(s.buf))
		var count uint32
		var bufPtr uintptr
		if size > 0 {
			bufPtr = uintptr(unsafe.Pointer(&s.buf[0]))
		}
		status, _, _ := procPdhGetFormattedCounterArray.Call(s.counter, format, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), bufPtr)
		if status == pdhMoreData {
			// The instance set can grow between the sizing call and the read.
			s.buf = make([]byte, size+size/4+1024)
			continue
		}
		if status != 0 {
			return nil, fmt.Errorf("PdhGetFormattedCounterArray failed: 0x%08X", status)
		}
		if count == 0 {
			return nil, nil
		}
		items := unsafe.Slice((*pdhCounterValueItem)(unsafe.Pointer(&s.buf[0])), count)
		readings := make([]gpuEngineReading, 0, count)
		for _, item := range items {
			if item.Name == nil || (item.CStatus != pdhCStatusValid && item.CStatus != pdhCStatusNew) {
				continue
			}
			if reading, ok := parseGPUEngineInstance(utf16PtrToString(item.Name), item.Value); ok {
				readings = append(readings, reading)
			}
		}
		return readings, nil
	}
	return nil, fmt.Errorf("GPU Engine instance set kept growing")
}

func (s *gpuEngineSampler) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.query != 0 {
		procPdhCloseQuery.Call(s.query)
		s.query, s.counter = 0, 0
	}
}

var errGPUEnginesWarming = fmt.Errorf("GPU engine counters are warming up")

// summary collects once and aggregates; failures become an unavailable
// summary that degrades per-process GPU and engine lists with a reason.
func (s *gpuEngineSampler) summary() gpuEngineSummary {
	readings, err := s.collect()
	if err != nil {
		return unavailableGPUEngineSummary(err.Error())
	}
	return summarizeGPUEngines(readings)
}

// warmSummary is summary for the direct Collect path, which answers the
// agent's first requests before the sampler has a snapshot and shares one
// deadline across every collector. It never pays the one-time PDH open
// (measured ~185 ms on BBLWIN), since a first collection could only report
// warming up anyway; the slow lane opens the query on its first pass.
func (s *gpuEngineSampler) warmSummary() gpuEngineSummary {
	s.mu.Lock()
	opened := s.opened
	s.mu.Unlock()
	if !opened {
		return unavailableGPUEngineSummary(errGPUEnginesWarming.Error())
	}
	return s.summary()
}

// dxgiAdapters returns the cached adapter list, re-enumerating after
// dxgiAdapterTTL.
func (s *gpuEngineSampler) dxgiAdapters() []dxgiAdapterInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.adaptersAt.IsZero() && time.Since(s.adaptersAt) < dxgiAdapterTTL {
		return s.adapters
	}
	adapters, err := enumerateDXGIAdapters()
	s.adaptersAt = time.Now()
	s.adapterErr = err
	if err == nil {
		s.adapters = adapters
	}
	return s.adapters
}

// dxgiAdapterDesc1 mirrors DXGI_ADAPTER_DESC1.
type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	LuidLow               uint32
	LuidHigh              int32
	Flags                 uint32
}

// iidIDXGIFactory1 is {770aae78-f26f-4dba-a829-253c83d1b387}.
var iidIDXGIFactory1 = syscall.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}

// COM vtable slots: IUnknown::Release is 2, IDXGIFactory1::EnumAdapters1 is
// 12, IDXGIAdapter1::GetDesc1 is 10.
const (
	comRelease             = 2
	dxgiFactoryEnumAdapter = 12
	dxgiAdapterGetDesc1    = 10
	dxgiErrorNotFound      = 0x887A0002
)

// comMethod reads one function pointer from a COM object's vtable. Object
// pointers stay unsafe.Pointer from the out-parameter on, so no uintptr is
// ever turned back into a pointer.
func comMethod(object unsafe.Pointer, slot int) uintptr {
	vtable := *(*unsafe.Pointer)(object)
	return *(*uintptr)(unsafe.Add(vtable, slot*int(unsafe.Sizeof(uintptr(0)))))
}

// enumerateDXGIAdapters lists every adapter's description and LUID. It needs
// no window, device or COM apartment, so it works from the session-0 service.
func enumerateDXGIAdapters() ([]dxgiAdapterInfo, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	var factory unsafe.Pointer
	if hr, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); hr != 0 || factory == nil {
		return nil, fmt.Errorf("CreateDXGIFactory1 failed: 0x%08X", uint32(hr))
	}
	defer syscall.SyscallN(comMethod(factory, comRelease), uintptr(factory))
	var adapters []dxgiAdapterInfo
	for index := uintptr(0); index < 16; index++ {
		var adapter unsafe.Pointer
		hr, _, _ := syscall.SyscallN(comMethod(factory, dxgiFactoryEnumAdapter), uintptr(factory), index, uintptr(unsafe.Pointer(&adapter)))
		if uint32(hr) == dxgiErrorNotFound {
			break
		}
		if hr != 0 || adapter == nil {
			return adapters, fmt.Errorf("EnumAdapters1(%d) failed: 0x%08X", index, uint32(hr))
		}
		var desc dxgiAdapterDesc1
		hr, _, _ = syscall.SyscallN(comMethod(adapter, dxgiAdapterGetDesc1), uintptr(adapter), uintptr(unsafe.Pointer(&desc)))
		syscall.SyscallN(comMethod(adapter, comRelease), uintptr(adapter))
		if hr != 0 {
			continue
		}
		adapters = append(adapters, dxgiAdapterInfo{
			Name:     strings.TrimSpace(syscall.UTF16ToString(desc.Description[:])),
			LUID:     fmt.Sprintf("0x%08x_0x%08x", uint32(desc.LuidHigh), desc.LuidLow),
			Software: desc.Flags&dxgiAdapterFlagSoftware != 0,
		})
	}
	return adapters, nil
}
