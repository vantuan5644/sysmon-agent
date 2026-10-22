//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"
	"unsafe"
)

// Physical-drive discovery for the storage panel, through device IOCTLs and
// kernel32 rather than Windows' Storage Management API (MSFT_PhysicalDisk,
// MSFT_Partition, Get-Volume).
//
// That API is only as fast as the slowest disk attached. On BBLWIN (2026-10-22)
// a voice recorder that enumerates as "ZEPHYR USB DISK" (Actions Semiconductor,
// USB\VID_10D6&PID_B00B) took ~5 s to fail each SCSI MODE SENSE, and the
// provider sends that kind of query several times per discovery. Every
// MSFT_Partition enumeration or Get-Volume call then stalled ~15 s, the
// discovery script went from 2 s to 19-26 s, and the storage panel reported
// "powershell query timed out after 8s" for as long as the recorder stayed
// plugged in. The calls below answered in 0-8 ms on that same recorder.
//
// Every handle is opened with zero access rights, like readWindowsDiskPerformance:
// that needs no privilege, and the device descriptor and volume extents come
// from what the class drivers already hold. Two reads may wait on the device
// itself: the drive geometry (a removable drive re-reads its capacity, a
// READ CAPACITY that Windows' own media polling sends anyway, never a vendor
// pass-through command) and a volume's free space. A busy device can hold
// either for a long time - the same recorder took 13-23 s to answer the
// geometry query while LibreHardwareMonitor's drive detection was sending it
// commands - so both run as boundedReads with windowsDeviceReadBudget, and a
// drive that has not answered in time is listed with what is already known.

var procQueryDosDeviceW = kernel32.NewProc("QueryDosDeviceW")

const (
	// IOCTL_STORAGE_QUERY_PROPERTY, IOCTL_DISK_GET_DRIVE_GEOMETRY_EX and
	// IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS. All three are FILE_ANY_ACCESS, which
	// is what lets a zero-access handle ask for them.
	ioctlStorageQueryProperty       = 0x2D1400
	ioctlDiskGetDriveGeometryEx     = 0x700A0
	ioctlVolumeGetVolumeDiskExtents = 0x560000

	driveRemovable = 2

	// windowsDeviceReadBudget is how long one discovery waits for the drive
	// sizes and volume free space it asked for. Internal drives answer in
	// 0-8 ms; the recorder took 2.8-3.7 s for its first command after sitting
	// idle (it wakes from USB suspend), which is every discovery at a 5-minute
	// TTL. It stays under windowsStorageDiscoveryWait so the first sample
	// gets rows.
	windowsDeviceReadBudget = 5 * time.Second
)

var (
	windowsDriveSizeReads   boundedReads[uint64]
	windowsVolumeSpaceReads boundedReads[windowsVolumeSpace]
)

type windowsVolumeSpace struct {
	total uint64
	free  uint64
}

// discoverWindowsPhysicalDisks lists every physical drive with its model,
// size, bus type, and the drive-lettered volumes on it, leaving out the drives
// exclude names.
func discoverWindowsPhysicalDisks(exclude storageExclusions) ([]windowsPhysicalDisk, error) {
	numbers, err := windowsPhysicalDriveNumbers()
	if err != nil {
		return nil, err
	}
	// Identity first. It comes from the descriptor the storage stack cached
	// when the drive arrived, so an excluded drive is recognized before
	// anything is sent to it, and nothing ever is.
	var drives []windowsPhysicalDisk
	var driveNumbers []uint32
	for _, number := range numbers {
		row, err := readWindowsDiskIdentity(number)
		if err != nil || exclude.excludes(row.Model) {
			continue
		}
		drives = append(drives, row)
		driveNumbers = append(driveNumbers, number)
	}
	deadline := time.Now().Add(windowsDeviceReadBudget)
	// Every size read is in flight before the first wait, so one slow drive
	// costs the budget once rather than once per drive behind it.
	sizes := make([]*boundedRead[uint64], len(drives))
	for i, number := range driveNumbers {
		sizes[i] = windowsDriveSizeReads.start(drives[i].Name, func() (uint64, error) {
			return readWindowsDriveSize(number)
		})
	}
	rows := make([]windowsPhysicalDisk, 0, len(drives))
	byNumber := make(map[uint32]int, len(drives))
	for i, row := range drives {
		size, known, err := windowsDriveSizeReads.result(row.Name, sizes[i], deadline)
		if known && err != nil {
			// No media (an empty card-reader slot). Not storage to report.
			continue
		}
		// A drive that has never answered is still listed, with no size; the
		// dashboard leaves a zero size out.
		row.SizeBytes = size
		byNumber[driveNumbers[i]] = len(rows)
		rows = append(rows, row)
	}
	addWindowsDriveLetters(rows, byNumber, deadline)
	return rows, nil
}

func physicalDriveName(number uint32) string {
	return fmt.Sprintf("PhysicalDrive%d", number)
}

// windowsPhysicalDriveNumbers lists the \\.\PhysicalDriveN devices from the
// DOS device namespace. Drive numbers can have gaps after a removal, so this
// reads the names that exist instead of counting up from 0. The list held 44K
// characters on BBLWIN, hence the growing buffer.
func windowsPhysicalDriveNumbers() ([]uint32, error) {
	for size := 1 << 16; size <= 1<<22; size <<= 1 {
		buf := make([]uint16, size)
		n, _, err := procQueryDosDeviceW.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n != 0 {
			return physicalDriveNumbers(buf[:n]), nil
		}
		if !errors.Is(err, syscall.ERROR_INSUFFICIENT_BUFFER) {
			return nil, fmt.Errorf("QueryDosDeviceW failed: %v", err)
		}
	}
	return nil, errors.New("QueryDosDeviceW: device list larger than 4M characters")
}

// readWindowsDiskIdentity reads the drive's name, model and bus type, all from
// the device descriptor the storage stack cached when the drive arrived. A
// drive whose descriptor cannot be read is still listed, as an unnamed drive
// of unknown bus type.
func readWindowsDiskIdentity(number uint32) (windowsPhysicalDisk, error) {
	name := physicalDriveName(number)
	handle, err := openZeroAccess(`\\.\` + name)
	if err != nil {
		return windowsPhysicalDisk{}, err
	}
	defer syscall.CloseHandle(handle)

	row := windowsPhysicalDisk{Name: name, Mountpoints: []string{}}
	// STORAGE_PROPERTY_QUERY{StorageDeviceProperty, PropertyStandardQuery}: all
	// zeroes.
	query := make([]byte, 12)
	descriptor := make([]byte, 1024)
	if returned, err := deviceIoControl(handle, ioctlStorageQueryProperty, query, descriptor); err == nil {
		if parsed, err := parseStorageDeviceDescriptor(descriptor[:returned]); err == nil {
			row.Model = windowsDiskModel(parsed.Vendor, parsed.Product)
			row.BusType = parsed.BusType
		}
	}
	return row, nil
}

// readWindowsDriveSize reads the drive's capacity. It fails on a drive with
// no media, which is how an empty card-reader slot is left out.
func readWindowsDriveSize(number uint32) (uint64, error) {
	name := physicalDriveName(number)
	handle, err := openZeroAccess(`\\.\` + name)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(handle)
	geometry := make([]byte, 256)
	returned, err := deviceIoControl(handle, ioctlDiskGetDriveGeometryEx, nil, geometry)
	if err != nil {
		return 0, fmt.Errorf("IOCTL_DISK_GET_DRIVE_GEOMETRY_EX on %s: %w", name, err)
	}
	if returned < 32 {
		return 0, fmt.Errorf("%s returned a %d-byte geometry", name, returned)
	}
	// DISK_GEOMETRY_EX: a 24-byte DISK_GEOMETRY, then DiskSize.
	size := binary.LittleEndian.Uint64(geometry[24:])
	if size == 0 {
		return 0, fmt.Errorf("%s reports no media", name)
	}
	return size, nil
}

// addWindowsDriveLetters attributes each lettered volume's capacity to the
// physical drive(s) it lives on. A volume spanning several drives counts on
// each of them, as the drive-centric panel lists what each drive serves.
// Folder mount points and letterless volumes are not shown, and neither is a
// volume whose free space never came back.
func addWindowsDriveLetters(rows []windowsPhysicalDisk, byNumber map[uint32]int, deadline time.Time) {
	type lettered struct {
		letter string
		disks  []uint32
		space  *boundedRead[windowsVolumeSpace]
	}
	var volumes []lettered
	mask, _, _ := procGetLogicalDrives.Call()
	for drive := 0; drive < 26; drive++ {
		if mask&(uintptr(1)<<drive) == 0 {
			continue
		}
		letter := fmt.Sprintf("%c:", 'A'+drive)
		root, err := syscall.UTF16PtrFromString(letter + `\`)
		if err != nil {
			continue
		}
		if kind, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(root))); kind != driveFixed && kind != driveRemovable {
			continue
		}
		disks, err := windowsVolumeDiskNumbers(letter)
		if err != nil || !slices.ContainsFunc(disks, func(disk uint32) bool { _, ok := byNumber[disk]; return ok }) {
			// Not a disk volume, or one on a drive that is not listed (an
			// excluded one): its free space is never asked for.
			continue
		}
		volumes = append(volumes, lettered{letter, disks, windowsVolumeSpaceReads.start(letter, func() (windowsVolumeSpace, error) {
			return readWindowsVolumeSpace(root)
		})})
	}
	for _, volume := range volumes {
		space, known, err := windowsVolumeSpaceReads.result(volume.letter, volume.space, deadline)
		if !known || err != nil || space.total == 0 {
			continue
		}
		used := space.total - min(space.free, space.total)
		for _, disk := range volume.disks {
			i, ok := byNumber[disk]
			if !ok {
				continue
			}
			rows[i].UsedBytes += used
			rows[i].TotalBytes += space.total
			rows[i].Mountpoints = append(rows[i].Mountpoints, volume.letter)
		}
	}
}

// ExcludeStorage sets the drives the agent leaves alone (-storage-exclude).
// main calls it once, before the sampler starts.
func (c *systemCollector) ExcludeStorage(exclude storageExclusions) {
	c.mu.Lock()
	c.storageExclude = exclude
	c.mu.Unlock()
}

const (
	// storageDeviceTemperatureProperty is STORAGE_PROPERTY_ID 52,
	// StorageDeviceTemperatureProperty.
	storageDeviceTemperatureProperty = 52
	// windowsDriveTemperatureTTL is how long a drive reading is reused. Drive
	// temperatures move over tens of seconds.
	windowsDriveTemperatureTTL = 15 * time.Second
	// windowsDriveTemperatureWait is how long a slow-lane pass waits for a due
	// reading before it shows the previous one. A read measured 1-6 ms.
	windowsDriveTemperatureWait = 250 * time.Millisecond
)

type cachedDriveTemperature struct {
	value NumberMetric
	at    time.Time
}

// windowsDriveTemperature reports the whole-drive temperature of an NVMe or
// SATA drive, read by readWindowsDriveTemperature. Every other drive says why
// it has none (driveTemperatureReadable): a USB one cannot pass its SMART log
// through the enclosure's bridge at all. A SATA drive that does not report a
// temperature to Windows says so as an expected condition; an NVMe drive
// always can, so a failure there stays an error.
//
// This replaced LibreHardwareMonitor's storage group as the source. Reading
// drives through LHM meant DiskInfoToolkit's detection, which sends vendor
// pass-through commands to every USB disk it cannot identify; see the drive
// temperature note in lhm-bridge-daemon.ps1.
func (c *systemCollector) windowsDriveTemperature(row windowsPhysicalDisk) NumberMetric {
	if !driveTemperatureReadable(row.BusType) {
		return unavailableNumber("C", storageTemperatureUnavailableReason(row.BusType))
	}
	now := time.Now()
	c.mu.Lock()
	cached, ok := c.driveTemps[row.Name]
	read := c.readDriveTemp
	c.mu.Unlock()
	if ok && now.Sub(cached.at) < windowsDriveTemperatureTTL {
		return cached.value
	}
	if read == nil {
		read = readWindowsDriveTemperature
	}
	pending := c.driveTempReads.start(row.Name, func() (float64, error) { return read(row.Name) })
	celsius, known, err := c.driveTempReads.result(row.Name, pending, now.Add(windowsDriveTemperatureWait))
	if !known {
		return unavailableNumber("C", "drive temperature not read yet")
	}
	value := availableNumber(celsius, "C")
	switch {
	case err == nil:
	case row.BusType != storageBusTypeNVMe && windowsDriveTemperatureUnsupported(err):
		value = unavailableNumber("C", noDriveTemperatureReason)
	default:
		value = unavailableNumber("C", err.Error())
	}
	c.mu.Lock()
	if c.driveTemps == nil {
		c.driveTemps = map[string]cachedDriveTemperature{}
	}
	c.driveTemps[row.Name] = cachedDriveTemperature{value: value, at: now}
	c.mu.Unlock()
	return value
}

// windowsDriveTemperatureUnsupported reports whether a temperature query
// failed because the drive or its driver has no temperature to give, as
// opposed to failing to answer: ERROR_INVALID_FUNCTION, ERROR_NOT_SUPPORTED,
// ERROR_INVALID_PARAMETER, or a reply listing no sensors.
func windowsDriveTemperatureUnsupported(err error) bool {
	return errors.Is(err, syscall.Errno(1)) || errors.Is(err, syscall.Errno(50)) ||
		errors.Is(err, syscall.Errno(87)) || errors.Is(err, errNoTemperatureSensors)
}

// readWindowsDriveTemperature asks the drive for StorageDeviceTemperatureProperty
// through a zero-access handle, and Windows' own storage driver answers it. For
// NVMe that is stornvme reading the health log, a standard admin command
// (measured 1-6 ms on BBLWIN, exactly what LibreHardwareMonitor showed for the
// same drives). Whether a SATA drive answers depends on the drive and the AHCI
// driver, and is not verified here: BBLWIN has no SATA drive.
func readWindowsDriveTemperature(name string) (float64, error) {
	handle, err := openZeroAccess(`\\.\` + name)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(handle)
	query := make([]byte, 12)
	binary.LittleEndian.PutUint32(query, storageDeviceTemperatureProperty)
	out := make([]byte, 512)
	returned, err := deviceIoControl(handle, ioctlStorageQueryProperty, query, out)
	if err != nil {
		return 0, fmt.Errorf("drive temperature query on %s: %w", name, err)
	}
	return parseStorageTemperatureDescriptor(out[:returned])
}

func readWindowsVolumeSpace(root *uint16) (windowsVolumeSpace, error) {
	var freeAvailable, total, totalFree uint64
	if ok, _, err := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	); ok == 0 {
		return windowsVolumeSpace{}, fmt.Errorf("GetDiskFreeSpaceExW failed: %v", err)
	}
	return windowsVolumeSpace{total: total, free: totalFree}, nil
}

// windowsVolumeDiskNumbers maps a drive letter to the physical drives under
// it. A letter that is not a disk volume (a SUBST, a CD) fails here and is
// skipped by the caller.
func windowsVolumeDiskNumbers(letter string) ([]uint32, error) {
	handle, err := openZeroAccess(`\\.\` + letter)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(handle)
	// Room for 8 extents; a volume spanning more reports its count with
	// ERROR_MORE_DATA and gets one retry sized to it.
	out := make([]byte, 8+24*8)
	returned, err := deviceIoControl(handle, ioctlVolumeGetVolumeDiskExtents, nil, out)
	if errors.Is(err, syscall.ERROR_MORE_DATA) {
		out = make([]byte, 8+24*int(binary.LittleEndian.Uint32(out)))
		returned, err = deviceIoControl(handle, ioctlVolumeGetVolumeDiskExtents, nil, out)
	}
	if err != nil {
		return nil, fmt.Errorf("IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS on %s: %w", letter, err)
	}
	return parseVolumeDiskExtents(out[:returned])
}

func openZeroAccess(path string) (syscall.Handle, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	handle, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return syscall.InvalidHandle, fmt.Errorf("open %s: %w", path, err)
	}
	return handle, nil
}

func deviceIoControl(handle syscall.Handle, code uint32, in, out []byte) (uint32, error) {
	var inPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	var returned uint32
	err := syscall.DeviceIoControl(handle, code, inPtr, uint32(len(in)), &out[0], uint32(len(out)), &returned, nil)
	return returned, err
}
