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

	// ifFlagFilterInterface is the FilterInterface bit of MIB_IF_ROW2's
	// InterfaceAndOperStatusFlags (HardwareInterface is bit 0).
	ifFlagFilterInterface = 1 << 1
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
//
// GetIfTable2 also lists every NDIS filter module bound to an adapter as an
// interface of its own ("Ethernet-QoS Packet Scheduler-0000", "Ethernet-WFP
// Native MAC Layer LightWeight Filter-0000", ...), each carrying the adapter's
// own octet counts. Get-NetAdapterStatistics never listed them. Counting them
// made the NET total a multiple of the real traffic: on BBLWIN (2026-11-15)
// a 133 MB/s download on the one Ethernet port read 521 MB/s, four rows of the
// same bytes. Filter rows are skipped by their flag, not by name, because the
// names are localized and depend on which filter drivers are installed.
func windowsNetCounters(ctx context.Context) (map[string]netCounter, error) {
	counters := map[string]netCounter{}
	err := walkWindowsIfTable(ctx, func(row *mibIfRow2) {
		if row.InterfaceAndOperStatusFlags&ifFlagFilterInterface != 0 {
			return
		}
		counters[windowsIfRowName(row)] = netCounter{rxBytes: row.InOctets, txBytes: row.OutOctets}
	})
	if err != nil {
		return nil, err
	}
	return counters, nil
}

func windowsIfRowName(row *mibIfRow2) string {
	if name := syscall.UTF16ToString(row.Alias[:]); name != "" {
		return name
	}
	if name := syscall.UTF16ToString(row.Description[:]); name != "" {
		return name
	}
	return fmt.Sprintf("interface %d", row.InterfaceIndex)
}

// walkWindowsIfTable calls visit for every row GetIfTable2 returns, filter
// modules included. The row points into C memory freed on return.
func walkWindowsIfTable(ctx context.Context, visit func(row *mibIfRow2)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var table *byte
	status, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&table)))
	if status != 0 {
		return fmt.Errorf("GetIfTable2 failed: status %d", status)
	}
	if table == nil {
		return fmt.Errorf("GetIfTable2 returned a nil table")
	}
	defer procFreeMibTable.Call(uintptr(unsafe.Pointer(table)))

	count := *(*uint32)(unsafe.Pointer(table))
	rowOffset := unsafe.Offsetof(mibIfTable2Layout{}.FirstRow)
	rowSize := unsafe.Sizeof(mibIfRow2{})
	for i := uint32(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		visit((*mibIfRow2)(unsafe.Add(unsafe.Pointer(table), rowOffset+uintptr(i)*rowSize)))
	}
	return nil
}
