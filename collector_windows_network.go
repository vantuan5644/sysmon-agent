//go:build windows

package main

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	iphlpapi         = syscall.NewLazyDLL("iphlpapi.dll")
	procGetIfTable2  = iphlpapi.NewProc("GetIfTable2")
	procFreeMibTable = iphlpapi.NewProc("FreeMibTable")
)

const (
	ifMaxStringSize      = 256
	ifMaxPhysicalAddress = 32
)

type windowsGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// mibIfRow2 mirrors MIB_IF_ROW2 from netioapi.h. The Go field order and native
// alignment are significant because GetIfTable2 returns a C-allocated array of
// these structures.
type mibIfRow2 struct {
	InterfaceLuid               uint64
	InterfaceIndex              uint32
	InterfaceGuid               windowsGUID
	Alias                       [ifMaxStringSize + 1]uint16
	Description                 [ifMaxStringSize + 1]uint16
	PhysicalAddressLength       uint32
	PhysicalAddress             [ifMaxPhysicalAddress]byte
	PermanentPhysicalAddress    [ifMaxPhysicalAddress]byte
	Mtu                         uint32
	Type                        uint32
	TunnelType                  uint32
	MediaType                   uint32
	PhysicalMediumType          uint32
	AccessType                  uint32
	DirectionType               uint32
	InterfaceAndOperStatusFlags byte
	OperStatus                  uint32
	AdminStatus                 uint32
	MediaConnectState           uint32
	NetworkGuid                 windowsGUID
	ConnectionType              uint32
	TransmitLinkSpeed           uint64
	ReceiveLinkSpeed            uint64
	InOctets                    uint64
	InUcastPkts                 uint64
	InNUcastPkts                uint64
	InDiscards                  uint64
	InErrors                    uint64
	InUnknownProtos             uint64
	InUcastOctets               uint64
	InMulticastOctets           uint64
	InBroadcastOctets           uint64
	OutOctets                   uint64
	OutUcastPkts                uint64
	OutNUcastPkts               uint64
	OutDiscards                 uint64
	OutErrors                   uint64
	OutUcastOctets              uint64
	OutMulticastOctets          uint64
	OutBroadcastOctets          uint64
	OutQLen                     uint64
}

type mibIfTable2Layout struct {
	NumEntries uint32
	FirstRow   mibIfRow2
}

// windowsNetCounters reads 64-bit interface octet counters in-process. The old
// Get-NetAdapterStatistics implementation started PowerShell on every 1.5 s
// slow pass; GetIfTable2 supplies the same totals without a child process.
func windowsNetCounters(ctx context.Context) (map[string]netCounter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var table *byte
	status, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&table)))
	if status != 0 {
		return nil, fmt.Errorf("GetIfTable2 failed: status %d", status)
	}
	if table == nil {
		return nil, fmt.Errorf("GetIfTable2 returned a nil table")
	}
	defer procFreeMibTable.Call(uintptr(unsafe.Pointer(table)))

	count := *(*uint32)(unsafe.Pointer(table))
	rowOffset := unsafe.Offsetof(mibIfTable2Layout{}.FirstRow)
	rowSize := unsafe.Sizeof(mibIfRow2{})
	counters := make(map[string]netCounter, int(count))
	for i := uint32(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := (*mibIfRow2)(unsafe.Add(unsafe.Pointer(table), rowOffset+uintptr(i)*rowSize))
		name := syscall.UTF16ToString(row.Alias[:])
		if name == "" {
			name = syscall.UTF16ToString(row.Description[:])
		}
		if name == "" {
			name = fmt.Sprintf("interface %d", row.InterfaceIndex)
		}
		counters[name] = netCounter{rxBytes: row.InOctets, txBytes: row.OutOctets}
	}
	return counters, nil
}
