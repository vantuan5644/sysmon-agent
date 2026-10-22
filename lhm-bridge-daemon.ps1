# LibreHardwareMonitor long-lived bridge daemon for sysmon-agent.
#
# Companion to lhm-bridge.ps1. The one-shot script pays the full fixed startup
# cost on every slow-lane sample: spawn pwsh, load LibreHardwareMonitorLib.dll,
# Computer.Open() (which loads the ring0 driver and enumerates SuperIO / SMBus /
# PSU / CPU / GPU / memory sensors), prime for 400 ms, read once, exit. That is
# ~4-5 s warm and slower cold, almost entirely fixed cost - the actual Update()
# + read is sub-second.
#
# This daemon pays Open() ONCE, then answers one JSON object per line read from
# stdin so the Go agent (lhm_bridge_windows.go) can keep this process alive
# across samples and get sub-second reads. The JSON contract is identical to the
# one-shot script (same fields, same sensor-selection rules).
#
# Protocol:
#   - Startup: resolve + LoadFrom the DLL, New-LhmComputer, Open() (with the
#     PSU exclusive-handle retry from the one-shot), prime once (Update + 400 ms
#     settle). If startup fails (no DLL, Open() throws even with PSU disabled),
#     the per-read loop below keeps emitting a single unavailable error object
#     for every request, so the agent degrades identically to the one-shot
#     bridge instead of churning process restarts.
#   - "osd" updates CPU/GPU hardware only; "rescan" re-enumerates hot-pluggable
#     hardware (Update-HotPluggedHardware) and then answers like a full read;
#     other lines request the full sensor snapshot. Every mode writes one
#     compact JSON line and flushes.
#   - On a per-read exception: write one unavailable error object, flush, keep
#     looping. The agent decides whether to restart the process.
#   - On stdin EOF (ReadLine returns $null, i.e. the agent closed stdin for a
#     clean shutdown): exit 0.
#
# The agent sends "rescan" every N reads / M minutes so a hot-plugged USB PSU is
# picked up. It used to kill and respawn this process for that; see the
# lhmDaemonRescanReads comment in lhm_bridge_windows.go.
#
# Usage: pwsh -NoProfile -ExecutionPolicy Bypass -File lhm-bridge-daemon.ps1
#
# Editing this file requires a Go rebuild: it is //go:embed-ed into
# collector_windows.go the same way lhm-bridge.ps1 is, and written to a temp
# copy once per process (see lhmDaemonScriptPath).

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

function Find-LhmLibrary {
    $candidates = @(
        'C:\ProgramData\chocolatey\lib\librehardwaremonitor\tools\LibreHardwareMonitorLib.dll',
        'C:\Program Files\LibreHardwareMonitor\LibreHardwareMonitorLib.dll',
        'C:\Program Files (x86)\LibreHardwareMonitor\LibreHardwareMonitorLib.dll'
    )
    $wingetDir = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Packages'
    if (Test-Path $wingetDir) {
        Get-ChildItem -Path $wingetDir -Filter 'LibreHardwareMonitorLib.dll' -Recurse -ErrorAction SilentlyContinue |
            ForEach-Object { $candidates += $_.FullName }
    }
    foreach ($path in $candidates) {
        if ($path -and (Test-Path $path)) { return $path }
    }
    return $null
}

# Builds a LibreHardwareMonitor Computer with the sensor groups the bridge
# reads. Smart PSU telemetry (Corsair HXi/RMi, NZXT E, Seasonic, MSI MEG, etc.
# over USB) is opt-in via IsPsuEnabled, which only exists on current builds (the
# forked OpenHardwareMonitor lacks it), so the assignment is guarded.
function New-LhmComputer([bool]$EnablePsu) {
    $computer = New-Object LibreHardwareMonitor.Hardware.Computer
    $computer.IsCpuEnabled = $true
    $computer.IsGpuEnabled = $true
    $computer.IsMotherboardEnabled = $true
    $computer.IsMemoryEnabled = $true
    # Storage is never enabled; see the drive temperature note below.
    $computer.IsPowerMonitorEnabled = $true
    if ($EnablePsu -and $computer.PSObject.Properties['IsPsuEnabled']) {
        $computer.IsPsuEnabled = $true
    }
    return $computer
}

function New-ErrorObject([string]$message) {
    return @{ available = $false; error = $message; power = $null; cpu_clock = $null; cpu_clock_peak_core = $null; psu_output_power = $null; temperatures = @() }
}

# Some LibreHardwareMonitor temperature sensors expose static configuration
# (thermal trip limits, sensor resolution) rather than a live reading. They are
# not useful as dashboard temperatures, so filter them out and keep only live
# temperature channels.
#
# 'warning' / 'critical' / 'threshold' sensors carry configured limits, not
# readings. The measured case was the NVMe SMART composite thresholds (79-88 C
# next to live drive temperatures of 29-50 C), back when drive sensors came
# through LHM; publishing one would put a static limit where a live
# temperature belongs, the same class of bug as picking the wrong CPU die
# sensor. Kept identical in lhm-bridge.ps1 and lhm-bridge-daemon.ps1.
function Test-LiveTemperature([string]$sensorName) {
    if (-not $sensorName) { return $true }
    $n = $sensorName.ToLower()
    if ($n -match 'limit') { return $false }
    if ($n -match 'resolution') { return $false }
    if ($n -match 'warning') { return $false }
    if ($n -match 'critical') { return $false }
    if ($n -match 'threshold') { return $false }
    return $true
}

# Returns true when a temperature reading represents a live, populated sensor
# rather than an unpopulated header. ASUS/MSI/Gigabyte boards expose optional
# probe headers (Water In/Out, spare thermal headers) that LibreHardwareMonitor
# surfaces with a value of exactly 0 C (or a null that casts to 0) when no probe
# is plugged in. A running PC never reports a real temperature of exactly 0 C,
# so treat 0 as 'not connected' and drop it - the same sentinel the Go agent
# uses for 0 W CPU package power. The -50..150 C band catches obvious garbage.
function Test-PlausibleTemperature([double]$value) {
    if ($value -eq 0) { return $false }
    return ($value -ge -50 -and $value -le 150)
}

# Select only the explicit NVIDIA die-hotspot channel. Memory junction is separate.
function Get-NvidiaHotspot($hardware) {
    if (-not $hardware.HardwareType -or $hardware.HardwareType.ToString() -ne 'GpuNvidia') { return }
    $sensor = $hardware.Sensors | Where-Object {
        $_.SensorType -eq 'Temperature' -and $_.Name -match '^GPU Hot ?Spot$' -and
        $_.Value -ne $null -and (Test-PlausibleTemperature ([double]$_.Value))
    } | Select-Object -First 1
    if ($sensor) {
        @{ name = $hardware.Name; identifier = $hardware.Identifier.ToString(); value = [math]::Round([double]$sensor.Value, 2) }
    }
}


# --- Drive temperatures do not come from LibreHardwareMonitor. ---
#
# The storage group is never enabled. Enabling it runs DiskInfoToolkit's drive
# detection, which tries vendor ATA/NVMe pass-through commands on every USB disk
# it cannot identify, both at Open() and whenever a disk is plugged in. That
# reset a USB SD card reader mid-write (2026-10-09), and on 2026-10-22 an
# in-development USB recorder let every command time out, so Open() ran past
# 5 minutes and the agent's restart loop sent the commands again and again.
# LHM has no per-drive exclusion, and the drives with a useful temperature,
# NVMe and SATA, report it to Windows itself: the agent reads them with
# IOCTL_STORAGE_QUERY_PROPERTY (collector_windows_storage.go).

# Re-enumerates hardware that can be hot-plugged, inside the open Computer.
# LibreHardwareMonitor adds or removes a sensor group when its Is*Enabled flag
# flips on an open Computer, so cycling IsPsuEnabled rebuilds only the USB PSU
# groups. It also retries a PSU that another process held exclusively at
# startup, which is what made Open() fall back to New-LhmComputer $false.
function Update-HotPluggedHardware($computer) {
    if ($computer.PSObject.Properties['IsPsuEnabled']) {
        try {
            $computer.IsPsuEnabled = $false
            $computer.IsPsuEnabled = $true
        }
        catch {
            # Still held elsewhere: leave PSU off and keep every other sensor.
            try { $computer.IsPsuEnabled = $false } catch {}
        }
    }
}

# Selects the aggregate output power (watts) from a PSU hardware node's Power
# sensors, or $null if none is reported. Vendor naming for the total rail
# output varies: Corsair HXi/RMi expose 'Output Power', NZXT/Seasonic 'Total
# Output', and the MSI MEG Ai series 'PSU Out'. Per-rail Power sensors such
# as '+12V' are excluded so an individual rail is never mistaken for the
# total. Preference order: an explicitly aggregate name ('output', 'out',
# 'total'), then the largest remaining non-rail Power sensor (the total is
# always >= any individual rail).
function Select-PsuOutputPower($sensors) {
    $railPattern = '^\s*\+?\s*\d+(\.\d+)?\s*v'
    $powers = @($sensors | Where-Object {
        $_.SensorType -eq 'Power' -and $_.Value -ne $null
    })
    if ($powers.Count -eq 0) { return $null }

    $aggregate = $powers | Where-Object {
        $n = $_.Name.ToLower()
        ($n -match 'output' -or $n -match '\bout\b' -or $n -match 'total') -and ($n -notmatch $railPattern)
    } | Select-Object -First 1
    if ($aggregate) { return [double]$aggregate.Value }

    $nonRail = @($powers | Where-Object { $_.Name -notmatch $railPattern })
    if ($nonRail.Count -ge 1) {
        $chosen = $nonRail | Sort-Object Value -Descending | Select-Object -First 1
        return [double]$chosen.Value
    }
    return $null
}

# True for the CPU hardware node. HardwareType is authoritative and is checked
# first because the name regex on its own also matches an AMD Radeon GPU node,
# which exposes its own Power and 'Core' Clock sensors and would otherwise be
# mistaken for the CPU when it enumerates first. The regex stays as the fallback
# for any build that does not surface HardwareType.
function Test-CpuNode($hw) {
    if ($hw.HardwareType) { return ($hw.HardwareType.ToString() -eq 'Cpu') }
    return ($hw.Name -match 'Ryzen|Intel|AMD|EPYC|Xeon|Core')
}

# Exact-name Power sensor lookup, or $null. Filtering on SensorType is not
# optional: the Zen 4 SMU table also defines a *Temperature* sensor named
# 'Package', so a name-only match can silently return degrees as watts.
function Select-PowerSensor($sensors, [string]$name) {
    $sensor = $sensors | Where-Object {
        $_.SensorType -eq 'Power' -and $_.Name -eq $name -and $_.Value -ne $null
    } | Select-Object -First 1
    if ($sensor) { return [double]$sensor.Value }
    return $null
}

# CPU package power, in preference order. The order is what makes our figure
# agree with AMD's own tools:
#   1. 'CPU PPT'     - AMD SMU telemetry (Package Power Tracking), read straight
#                      out of the SMU PM table. Instantaneous, and the same
#                      source and definition Ryzen Master and AMD Adrenalin
#                      report.
#   2. 'Total Power' - the other SMU socket-total slot; tracks PPT within ~1 W
#                      and covers parts where the PPT slot reads zero.
#   3. 'Package'     - RAPL (MSR_PKG_ENERGY_STAT). Always present on Zen and
#                      Intel, so it stays the fallback, but it is an energy
#                      accumulator delta divided by the wall time since the
#                      caller's previous Update(). That makes it an interval
#                      average rather than a reading, it overshoots on the first
#                      poll after Computer.Open(), and its 32-bit accumulator
#                      silently undercounts once a poll gap exceeds one wrap
#                      (~11 min at 100 W - LHM corrects only a single wrap).
#                      Measured ~10% above PPT on a 7950X.
function Select-CpuPackagePower($sensors) {
    foreach ($name in @('CPU PPT', 'Total Power', 'Package')) {
        $value = Select-PowerSensor $sensors $name
        if ($null -ne $value -and $value -gt 0) { return $value }
    }
    return $null
}

# One Update + read pass against an already-open Computer. Returns the compact
# JSON string the Go agent parses. Mirrors the sensor-selection logic of the
# one-shot script exactly (same field names, same selection rules) so the JSON
# contract is identical. Unlike the one-shot it does a SINGLE pass per call -
# no 3x retry / settle loop - because the daemon gets a fresh pass every slow
# lane tick, so a stale first reading self-heals on the next sample. This is
# what makes each read sub-second.
function Read-LhmSnapshot($computer) {
    $cpuPackagePower = $null
    $cpuCorePower = $null
    $cpuSocPower = $null
    $cpuMiscPower = $null
    $cpuClock = $null
    $cpuClockPeakCore = $null
    $psuOutputPower = $null
    $temperatures = New-Object System.Collections.Generic.List[object]
    $gpuHotspots = New-Object System.Collections.Generic.List[object]

    foreach ($hw in $computer.Hardware) {
        try { $hw.Update() } catch {}
        $sensors = @($hw.Sensors)
        foreach ($reading in @(Get-NvidiaHotspot $hw)) { $gpuHotspots.Add($reading) }
        # First CPU node that reports package power wins. The per-rail breakdown
        # is read from that same node so the parts always sum against the total
        # they were measured with.
        if (-not $cpuPackagePower -and (Test-CpuNode $hw)) {
            $pkg = Select-CpuPackagePower $sensors
            if ($null -ne $pkg) {
                $cpuPackagePower = $pkg
                # AMD SMU per-rail breakdown. 'Core Power' is the cores-only rail
                # and is the figure AMD Adrenalin displays as CPU power; package
                # power additionally includes SOC (IO die, memory controller,
                # Infinity Fabric) and Misc, which on a chiplet part add ~30 W
                # that Adrenalin never shows. Absent on Intel and on Zen parts
                # whose SMU PM-table version LHM has no sensor map for, so each
                # rail degrades independently to null.
                $cpuCorePower = Select-PowerSensor $sensors 'Core Power'
                $cpuSocPower = Select-PowerSensor $sensors 'SOC Power'
                $cpuMiscPower = Select-PowerSensor $sensors 'Misc Power'
            }
        }
        # Live CPU clock, emitted as TWO figures in MHz (selection rules kept
        # line-for-line identical to lhm-bridge.ps1 -- see the longer rationale
        # there): cpu_clock is the cross-core average, which is what the dashboard
        # shows live; cpu_clock_peak_core is the fastest single core in the same
        # sample, which pins near the boost ceiling by design and is what the
        # agent ratchets its observed-ceiling peak-hold with. LHM reads the
        # per-core MSRs on each Update(), so both track real load. For the
        # average, prefer LHM's own 'Cores (Average)' aggregate; otherwise average
        # the individual 'Core #N' clocks. Per-domain ('Bus Speed', 'Fabric',
        # 'Memory', 'Uncore') and '(Effective)' sensors are excluded - effective
        # clocks collapse toward 0 in idle C-states and would read as a misleading
        # sub-GHz value.
        if ($null -eq $cpuClock -and (Test-CpuNode $hw)) {
            $clockSensors = @($sensors | Where-Object { $_.SensorType -eq 'Clock' -and $_.Value -ne $null })
            # Individual 'Core #N' clocks, gathered unconditionally -- the per-core
            # peak is the maximum of these and cannot be recovered from the
            # 'Cores (Average)' aggregate, so this runs even when that aggregate
            # exists and supplies the average.
            $coreClocks = @($clockSensors | Where-Object {
                $_.Name -match 'Core' -and $_.Name -notmatch 'Effective' -and $_.Name -notmatch 'Average'
            } | ForEach-Object { [double]$_.Value } | Where-Object { $_ -gt 0 })
            $avg = $clockSensors | Where-Object {
                $_.Name -match 'Average' -and $_.Name -notmatch 'Effective' -and $_.Name -match 'Core'
            } | Select-Object -First 1
            if ($avg) {
                $cpuClock = [double]$avg.Value
            } elseif ($coreClocks.Count -gt 0) {
                $cpuClock = ($coreClocks | Measure-Object -Average).Average
            }
            if ($coreClocks.Count -gt 0) {
                $cpuClockPeakCore = ($coreClocks | Measure-Object -Maximum).Maximum
            }
        }
        # PSU total output power: only hardware LHM classifies as a PSU
        # (HardwareType 'Psu') is considered. The aggregate rail output sensor
        # is resolved by Select-PsuOutputPower because vendor naming varies.
        if (-not $psuOutputPower -and $hw.HardwareType -and $hw.HardwareType.ToString() -eq 'Psu') {
            $psuOutputPower = Select-PsuOutputPower $sensors
        }
        foreach ($s in $sensors) {
            if ($s.SensorType -eq 'Temperature' -and $s.Value -ne $null) {
                $temp = [double]$s.Value
                if ((Test-PlausibleTemperature $temp) -and (Test-LiveTemperature $s.Name)) {
                    $temperatures.Add(@{ name = ($hw.Name + ' ' + $s.Name).Trim(); value = [math]::Round($temp, 2) })
                }
            }
        }
        foreach ($sub in $hw.SubHardware) {
            try { $sub.Update() } catch {}
            # Some vendor USB controllers expose the PSU as a subhardware node.
            if (-not $psuOutputPower -and $sub.HardwareType -and $sub.HardwareType.ToString() -eq 'Psu') {
                $psuOutputPower = Select-PsuOutputPower @($sub.Sensors)
            }
            foreach ($s in $sub.Sensors) {
                if ($s.SensorType -eq 'Temperature' -and $s.Value -ne $null) {
                    $temp = [double]$s.Value
                    if ((Test-PlausibleTemperature $temp) -and (Test-LiveTemperature $s.Name)) {
                        $temperatures.Add(@{ name = ($hw.Name + ' ' + $sub.Name + ' ' + $s.Name).Trim(); value = [math]::Round($temp, 2) })
                    }
                }
            }
        }
    }

    $result = @{
        available        = $true
        power            = if ($null -ne $cpuPackagePower) { @{ available = $true; value = [math]::Round($cpuPackagePower, 2) } } else { $null }
        cpu_core_power   = if ($null -ne $cpuCorePower) { @{ available = $true; value = [math]::Round($cpuCorePower, 2) } } else { $null }
        cpu_soc_power    = if ($null -ne $cpuSocPower) { @{ available = $true; value = [math]::Round($cpuSocPower, 2) } } else { $null }
        cpu_misc_power   = if ($null -ne $cpuMiscPower) { @{ available = $true; value = [math]::Round($cpuMiscPower, 2) } } else { $null }
        cpu_clock        = if ($null -ne $cpuClock -and $cpuClock -gt 0) { @{ available = $true; value = [math]::Round($cpuClock, 0) } } else { $null }
        cpu_clock_peak_core = if ($null -ne $cpuClockPeakCore -and $cpuClockPeakCore -gt 0) { @{ available = $true; value = [math]::Round($cpuClockPeakCore, 0) } } else { $null }
        psu_output_power = if ($null -ne $psuOutputPower) { @{ available = $true; value = [math]::Round($psuOutputPower, 2) } } else { $null }
        gpu_hotspots     = $gpuHotspots
        temperatures     = $temperatures
    }
    return $result | ConvertTo-Json -Compress -Depth 6
}

# OSD requests update only CPU/GPU nodes. They use the same already-open
# Computer and skip storage, motherboard, PSU, and the full result traversal.
function Read-LhmOsdSnapshot($computer) {
    $temperatures = New-Object System.Collections.Generic.List[object]
    $devices = New-Object System.Collections.Generic.List[object]
    foreach ($hw in $computer.Hardware) {
        $cpu = Test-CpuNode $hw
        $gpu = $hw.HardwareType.ToString() -match '^Gpu(Nvidia|Amd|Intel)$'
        if (-not $cpu -and -not $gpu) { continue }
        $updated = $true
        try { $hw.Update() } catch { $updated = $false }
        $sensors = @()
        if ($updated) {
            $sensors = @($hw.Sensors)
            foreach ($sub in $hw.SubHardware) {
                try { $sub.Update(); $sensors += @($sub.Sensors) } catch {}
            }
        }
        if ($cpu) {
            foreach ($sensor in $sensors) {
                if ($sensor.SensorType -eq 'Temperature' -and $null -ne $sensor.Value -and
                    (Test-PlausibleTemperature ([double]$sensor.Value)) -and (Test-LiveTemperature $sensor.Name)) {
                    $temperatures.Add(@{name=($hw.Name + ' ' + $sensor.Name).Trim();value=[math]::Round([double]$sensor.Value,2)})
                }
            }
        }
        if ($gpu) {
            $load = $sensors | Where-Object { $_.SensorType -eq 'Load' -and $_.Name -eq 'GPU Core' -and $null -ne $_.Value } | Select-Object -First 1
            $temp = $sensors | Where-Object { $_.SensorType -eq 'Temperature' -and $_.Name -eq 'GPU Core' -and $null -ne $_.Value } | Select-Object -First 1
            $usage = @{available=$false;value=0;unit='%';error='GPU core load unavailable'}
            $temperature = @{available=$false;value=0;unit='C';error='GPU core temperature unavailable'}
            if ($load -and [double]$load.Value -ge 0 -and [double]$load.Value -le 100) {
                $usage = @{available=$true;value=[math]::Round([double]$load.Value,2);unit='%'}
            }
            if ($temp -and (Test-PlausibleTemperature ([double]$temp.Value))) {
                $temperature = @{available=$true;value=[math]::Round([double]$temp.Value,2);unit='C'}
            }
            $devices.Add(@{name=$hw.Name;usage_percent=$usage;temperature_celsius=$temperature})
        }
    }
    return @{available=$true;temperatures=$temperatures;gpu=$devices} | ConvertTo-Json -Compress -Depth 6
}

# --- Startup: Open() the Computer once and prime it. ---
$dll = Find-LhmLibrary
$computer = $null
$startupError = $null
if (-not $dll) {
    $startupError = 'LibreHardwareMonitor not installed (run: choco install librehardwaremonitor)'
} else {
    try {
        [void][System.Reflection.Assembly]::LoadFrom($dll)
        $computer = New-LhmComputer $true
        try {
            $computer.Open()
        }
        catch {
            # Computer.Open() can throw when a USB-linked PSU controller (Corsair
            # Link, MSI MEG Ai, NZXT E, Seasonic) is already held exclusively by
            # another process - most often the LibreHardwareMonitor GUI. Re-open a
            # fresh Computer with PSU disabled so the CPU/GPU/memory/motherboard
            # sensors still load; the agent then reports PSU power as unavailable
            # instead of losing every sensor. If the kernel driver itself is
            # locked, the second Open() rethrows into the outer catch and the
            # daemon degrades to emitting an unavailable error object per request.
            try { $computer.Close() } catch {}
            $computer = New-LhmComputer $false
            $computer.Open()
        }
        # Prime the sensors before serving reads. The first reading after Open()
        # frequently returns stale or zero values while the kernel driver warms
        # up, so poll every hardware (and subhardware) once and let it settle.
        foreach ($hw in $computer.Hardware) {
            try { $hw.Update() } catch {}
            foreach ($sub in $hw.SubHardware) {
                try { $sub.Update() } catch {}
            }
        }
        Start-Sleep -Milliseconds 400
    }
    catch {
        $startupError = $_.Exception.Message
        $computer = $null
    }
}

# --- Request loop: one JSON object per stdin line. ---
# [Console]::In.ReadLine() blocks until a line arrives and returns $null on EOF
# (the agent closed stdin), which is the clean-shutdown signal. Write each
# response via [Console]::Out (not the pipeline / Write-Output, which buffers
# under redirected stdout) and flush so the agent's deadline-bounded read gets
# the line promptly.
while ($null -ne ($line = [Console]::In.ReadLine())) {
    try {
        if ($startupError) {
            $json = New-ErrorObject $startupError | ConvertTo-Json -Compress -Depth 6
        } elseif ($line -eq 'osd') {
            $json = Read-LhmOsdSnapshot $computer
        } elseif ($line -eq 'rescan') {
            Update-HotPluggedHardware $computer
            $json = Read-LhmSnapshot $computer
        } else {
            $json = Read-LhmSnapshot $computer
        }
    }
    catch {
        $json = New-ErrorObject $_.Exception.Message | ConvertTo-Json -Compress -Depth 6
    }
    [Console]::Out.WriteLine($json)
    [Console]::Out.Flush()
}
exit 0
