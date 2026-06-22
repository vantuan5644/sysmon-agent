//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"
)

// Minimal Windows Service Control Manager (SCM) integration implemented with
// advapi32/kernel32 syscalls, so the agent runs as a native service while the
// module stays dependency-free (go.mod is stdlib-only). This mirrors the small
// subset of golang.org/x/sys/windows/svc we need: connect to the SCM, report
// SERVICE_RUNNING, and turn the STOP/SHUTDOWN controls into a stop signal that
// drives the same graceful shutdown the console path uses. Without this the
// process is a plain console server, so the SCM never gets the RUNNING
// handshake and Start-Service fails with error 1053 ("did not respond ... in a
// timely fashion").

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procStartServiceCtrlDispatcherW   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus              = advapi32.NewProc("SetServiceStatus")
	procGetCurrentProcessId           = kernel32.NewProc("GetCurrentProcessId")
	procProcessIdToSessionId          = kernel32.NewProc("ProcessIdToSessionId")
)

const (
	serviceWin32OwnProcess = 0x00000010

	serviceStopped      = 0x00000001
	serviceStartPending = 0x00000002
	serviceStopPending  = 0x00000003
	serviceRunning      = 0x00000004

	serviceControlStop        = 0x00000001
	serviceControlInterrogate = 0x00000004
	serviceControlShutdown    = 0x00000005

	serviceAcceptStop     = 0x00000001
	serviceAcceptShutdown = 0x00000004

	errorFailedServiceControllerConnect = 1063

	servicePendingWaitHintMs = 15000
)

// serviceStatus mirrors the Win32 SERVICE_STATUS struct passed to
// SetServiceStatus.
type serviceStatus struct {
	dwServiceType             uint32
	dwCurrentState            uint32
	dwControlsAccepted        uint32
	dwWin32ExitCode           uint32
	dwServiceSpecificExitCode uint32
	dwCheckPoint              uint32
	dwWaitHint                uint32
}

// serviceTableEntry mirrors the Win32 SERVICE_TABLE_ENTRY struct. The dispatch
// table passed to StartServiceCtrlDispatcher is terminated by a zeroed entry.
type serviceTableEntry struct {
	name        *uint16
	serviceProc uintptr
}

var (
	svcMu         sync.Mutex
	svcHandle     uintptr
	svcCheckPoint uint32

	svcRun       serviceRunFunc
	svcStop      chan struct{}
	svcStopOnce  sync.Once
	svcRunResult error
)

// runAsService connects the process to the SCM and blocks until the service is
// asked to stop. It returns (true, runErr) when the process was launched by the
// SCM, or (false, nil) when it is running interactively and should fall back to
// the console path.
func runAsService(name string, run serviceRunFunc) (bool, error) {
	if !startedByServiceControlManager() {
		return false, nil
	}

	svcRun = run

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return true, err
	}
	// Keep the dispatch table (and the name it points at) alive for the whole
	// blocking dispatcher call.
	table := []serviceTableEntry{
		{name: namePtr, serviceProc: syscall.NewCallback(serviceMain)},
		{},
	}
	r, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && uintptr(errno) == errorFailedServiceControllerConnect {
			// Not actually launched by the SCM after all (e.g. a session-0
			// console run); fall back to the console path.
			return false, nil
		}
		return true, callErr
	}
	return true, svcRunResult
}

// startedByServiceControlManager returns true when the process is most likely a
// service. Interactive console processes run in a session greater than 0; native
// services run in session 0. This short-circuits the common interactive case so
// we never invoke the SCM dispatcher there; runAsService still treats
// ERROR_FAILED_SERVICE_CONTROLLER_CONNECT as the authoritative "not a service"
// signal for the session-0 edge cases.
func startedByServiceControlManager() bool {
	pid, _, _ := procGetCurrentProcessId.Call()
	var sessionID uint32
	r, _, _ := procProcessIdToSessionId.Call(pid, uintptr(unsafe.Pointer(&sessionID)))
	if r == 0 {
		// Could not determine the session; let the dispatcher decide.
		return true
	}
	return sessionID == 0
}

// serviceMain is the SERVICE_MAIN_FUNCTION the SCM dispatcher invokes on its own
// thread. It registers the control handler, runs the agent (reporting RUNNING
// once the listener is up), and reports STOPPED when the agent exits.
func serviceMain(argc uint32, argv **uint16) uintptr {
	namePtr, _ := syscall.UTF16PtrFromString(serviceName)
	handle, _, _ := procRegisterServiceCtrlHandlerExW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		syscall.NewCallback(serviceHandler),
		0,
	)
	if handle == 0 {
		return 0
	}

	svcMu.Lock()
	svcHandle = handle
	svcMu.Unlock()

	svcStop = make(chan struct{})

	reportStatus(serviceStartPending, 0, servicePendingWaitHintMs)

	ready := func() {
		reportStatus(serviceRunning, serviceAcceptStop|serviceAcceptShutdown, 0)
	}

	svcRunResult = svcRun(svcStop, ready)

	if svcRunResult != nil {
		reportStatusExit(serviceStopped, 1)
	} else {
		reportStatus(serviceStopped, 0, 0)
	}
	return 0
}

// serviceHandler is the HANDLER_FUNCTION_EX the SCM calls (on a separate thread)
// to deliver control requests. STOP and SHUTDOWN trigger a graceful shutdown via
// the stop channel.
func serviceHandler(control uint32, eventType uint32, eventData uintptr, context uintptr) uintptr {
	switch control {
	case serviceControlStop, serviceControlShutdown:
		reportStatus(serviceStopPending, 0, servicePendingWaitHintMs)
		svcStopOnce.Do(func() { close(svcStop) })
	case serviceControlInterrogate:
		// The SCM expects the current status echoed back; nothing else to do.
	}
	return 0 // NO_ERROR
}

func reportStatus(state, accepts, waitHint uint32) {
	svcMu.Lock()
	defer svcMu.Unlock()
	if svcHandle == 0 {
		return
	}
	if state == serviceStartPending || state == serviceStopPending {
		svcCheckPoint++
	} else {
		svcCheckPoint = 0
	}
	status := serviceStatus{
		dwServiceType:      serviceWin32OwnProcess,
		dwCurrentState:     state,
		dwControlsAccepted: accepts,
		dwCheckPoint:       svcCheckPoint,
		dwWaitHint:         waitHint,
	}
	procSetServiceStatus.Call(svcHandle, uintptr(unsafe.Pointer(&status)))
}

func reportStatusExit(state, exitCode uint32) {
	svcMu.Lock()
	defer svcMu.Unlock()
	if svcHandle == 0 {
		return
	}
	status := serviceStatus{
		dwServiceType:   serviceWin32OwnProcess,
		dwCurrentState:  state,
		dwWin32ExitCode: exitCode,
	}
	procSetServiceStatus.Call(svcHandle, uintptr(unsafe.Pointer(&status)))
}
