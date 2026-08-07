# LibreHardwareMonitor bridge for sysmon-agent.
#
# Loads LibreHardwareMonitorLib.dll directly (no GUI, no WMI provider needed)
# and emits one JSON object the Go agent parses. Resolves the library from the
# standard install locations so the same script works after a WinGet or choco
# install:
#   - choco:  C:\ProgramData\chocolatey\lib\librehardwaremonitor\tools (stable)
#   - WinGet: %LOCALAPPDATA%\Microsoft\WinGet\Packages\LibreHardwareMonitor.* (per-user)
#   - manual: C:\Program Files\LibreHardwareMonitor (portable copy)
#
# Returns JSON to stdout. On any error, writes a single error object and exits 0
# so the agent can degrade gracefully instead of parsing stderr as JSON.
#
# Usage: pwsh -NoProfile -ExecutionPolicy Bypass -File lhm-bridge.ps1
#
# Run elevated once after install so the LibreHardwareMonitor kernel driver is
# loaded; after the first elevated run the driver stays available for the boot
# session, so subsequent runs from a normal agent account keep working.

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
# over USB) is opt-in via IsPsuEnabled, which only exists on current builds
# (the forked OpenHardwareMonitor lacks it), so the assignment is guarded.
function New-LhmComputer([bool]$EnablePsu) {
    $computer = New-Object LibreHardwareMonitor.Hardware.Computer
    $computer.IsCpuEnabled = $true
    $computer.IsGpuEnabled = $true
    $computer.IsMotherboardEnabled = $true
    $computer.IsMemoryEnabled = $true
    # Storage enables NVMe/SATA SMART temperature reads so per-drive storage temps
    # surface with no Go change (the temperature harvest loop is already generic).
    if ($computer.PSObject.Properties['IsStorageEnabled']) {
        $computer.IsStorageEnabled = $true
    }
    $computer.IsPowerMonitorEnabled = $true
    if ($EnablePsu -and $computer.PSObject.Properties['IsPsuEnabled']) {
        $computer.IsPsuEnabled = $true
    }
    return $computer
}

function Emit-Error([string]$message) {
    @{ available = $false; error = $message; power = $null; cpu_clock = $null; psu_output_power = $null; temperatures = @() } |
        ConvertTo-Json -Compress -Depth 6
    exit 0
}

# Some LibreHardwareMonitor temperature sensors expose static configuration
# (thermal trip limits, sensor resolution) rather than a live reading. They are
# not useful as dashboard temperatures, so filter them out and keep only live
# temperature channels.
function Test-LiveTemperature([string]$sensorName) {
    if (-not $sensorName) { return $true }
    $n = $sensorName.ToLower()
    if ($n -match 'limit') { return $false }
    if ($n -match 'resolution') { return $false }
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

# CPU package power, in preference order. Kept identical to the daemon script so
# the two bridges emit the same number. The order is what makes our figure agree
# with AMD's own tools:
#   1. 'CPU PPT'     - AMD SMU telemetry (Package Power Tracking), read straight
#                      out of the SMU PM table. Instantaneous, and the same
#                      source and definition Ryzen Master and AMD Adrenalin
#                      report.
#   2. 'Total Power' - the other SMU socket-total slot; tracks PPT within ~1 W
#                      and covers parts where the PPT slot reads zero.
#   3. 'Package'     - RAPL (MSR_PKG_ENERGY_STAT). Always present on Zen and
#                      Intel, so it stays the fallback, but it is an energy
#                      accumulator delta divided by the wall time since the
#                      caller's previous Update(). That matters most here: this
#                      one-shot script Opens a fresh Computer every call, and the
#                      first RAPL reading after Open() measured 112-120 W on a
#                      7950X where steady state was ~98 W. The SMU rails carry no
#                      such warm-up error.
function Select-CpuPackagePower($sensors) {
    foreach ($name in @('CPU PPT', 'Total Power', 'Package')) {
        $value = Select-PowerSensor $sensors $name
        if ($null -ne $value -and $value -gt 0) { return $value }
    }
    return $null
}

$dll = Find-LhmLibrary
if (-not $dll) {
    Emit-Error 'LibreHardwareMonitor not installed (run: choco install librehardwaremonitor)'
}

try {
    $lib = [System.Reflection.Assembly]::LoadFrom($dll)
    $computer = New-LhmComputer $true
    try {
        $computer.Open()
    }
    catch {
        # $computer.Open() can throw when a USB-linked PSU controller (Corsair
        # Link, MSI MEG Ai, NZXT E, Seasonic) is already held exclusively by
        # another process - most often the LibreHardwareMonitor GUI, which keeps
        # the HID handle open while it polls. That exception would otherwise
        # abort the whole bridge and take CPU package power and every temperature
        # down with it. Re-open a fresh Computer with PSU disabled so the
        # CPU/GPU/memory/motherboard sensors still load; the agent then reports
        # PSU power as unavailable instead of losing every sensor. If the kernel
        # driver itself is locked, the second Open() rethrows into the outer
        # catch below and the bridge degrades to a single unavailable error.
        try { $computer.Close() } catch {}
        $computer = New-LhmComputer $false
        $computer.Open()
    }

    # Prime the sensors before reading. The first reading after Open()
    # frequently returns stale or zero values while the kernel driver warms
    # up, so poll every hardware (and subhardware) once and let it settle.
    foreach ($hw in $computer.Hardware) {
        try { $hw.Update() } catch {}
        foreach ($sub in $hw.SubHardware) {
            try { $sub.Update() } catch {}
        }
    }
    Start-Sleep -Milliseconds 400

    $cpuPackagePower = $null
    $cpuCorePower = $null
    $cpuSocPower = $null
    $cpuMiscPower = $null
    $cpuClock = $null
    $psuOutputPower = $null
    $temperatures = New-Object System.Collections.Generic.List[object]

    # Read with retry. LibreHardwareMonitor's kernel driver can hand back a
    # partial or stale reading (0 W package power, only a handful of sensors)
    # while it warms up or after another host process has been cycling it.
    # Re-running Update() on the already-open Computer recovers the full sensor
    # set, mirroring how the LHM GUI polls a persistent open instead of
    # reopening. Up to three passes; the loop breaks early on a sane reading.
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        $cpuPackagePower = $null
        $cpuCorePower = $null
        $cpuSocPower = $null
        $cpuMiscPower = $null
        $cpuClock = $null
        $psuOutputPower = $null
        $temperatures.Clear()
        foreach ($hw in $computer.Hardware) {
            try { $hw.Update() } catch {}
            $sensors = @($hw.Sensors)
            # First CPU node that reports package power wins. The per-rail
            # breakdown is read from that same node so the parts always sum
            # against the total they were measured with.
            if (-not $cpuPackagePower -and (Test-CpuNode $hw)) {
                $pkg = Select-CpuPackagePower $sensors
                if ($null -ne $pkg) {
                    $cpuPackagePower = $pkg
                    # AMD SMU per-rail breakdown. 'Core Power' is the cores-only
                    # rail and is the figure AMD Adrenalin displays as CPU power;
                    # package power additionally includes SOC (IO die, memory
                    # controller, Infinity Fabric) and Misc, which on a chiplet
                    # part add ~30 W that Adrenalin never shows. Absent on Intel
                    # and on Zen parts whose SMU PM-table version LHM has no
                    # sensor map for, so each rail degrades independently.
                    $cpuCorePower = Select-PowerSensor $sensors 'Core Power'
                    $cpuSocPower = Select-PowerSensor $sensors 'SOC Power'
                    $cpuMiscPower = Select-PowerSensor $sensors 'Misc Power'
                }
            }
            # Live CPU clock: the average per-core clock in MHz. LHM reads the
            # per-core MSRs on each Update(), so unlike Win32_Processor's static
            # CurrentClockSpeed this tracks real load. The average (not the max)
            # is used because on a many-core CPU some core is almost always
            # momentarily boosting, so the max pins at the boost ceiling and looks
            # just as constant as the WMI value. Prefer LHM's own 'Cores (Average)'
            # aggregate; otherwise average the individual 'Core #N' clocks. The
            # per-domain ('Bus Speed', 'Fabric', 'Memory', 'Uncore') and
            # '(Effective)' sensors are excluded - effective clocks collapse toward
            # 0 in idle C-states and would read as a misleading sub-GHz value.
            if ($null -eq $cpuClock -and (Test-CpuNode $hw)) {
                $clockSensors = @($sensors | Where-Object { $_.SensorType -eq 'Clock' -and $_.Value -ne $null })
                $avg = $clockSensors | Where-Object {
                    $_.Name -match 'Average' -and $_.Name -notmatch 'Effective' -and $_.Name -match 'Core'
                } | Select-Object -First 1
                if ($avg) {
                    $cpuClock = [double]$avg.Value
                } else {
                    $coreClocks = @($clockSensors | Where-Object {
                        $_.Name -match 'Core' -and $_.Name -notmatch 'Effective' -and $_.Name -notmatch 'Average'
                    } | ForEach-Object { [double]$_.Value } | Where-Object { $_ -gt 0 })
                    if ($coreClocks.Count -gt 0) {
                        $cpuClock = ($coreClocks | Measure-Object -Average).Average
                    }
                }
            }
            # PSU total output power: only hardware LHM classifies as a PSU
            # (HardwareType 'Psu') is considered, so CPU package or GPU board
            # power is never mistaken for the PSU. The aggregate rail output
            # sensor is resolved by Select-PsuOutputPower because vendor naming
            # varies (Corsair 'Output Power', NZXT 'Total Output', MSI 'PSU Out').
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
                # Some vendor USB controllers expose the PSU as a subhardware
                # node (rare), so check it here too.
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
        # Accept the reading once CPU package power is live and the sensor
        # population looks complete; otherwise settle briefly and retry.
        if ($cpuPackagePower -and $cpuPackagePower -gt 0 -and $temperatures.Count -ge 8) {
            break
        }
        if ($attempt -lt 3) { Start-Sleep -Milliseconds 300 }
    }
    $computer.Close()

    $result = @{
        available        = $true
        power            = if ($null -ne $cpuPackagePower) { @{ available = $true; value = [math]::Round($cpuPackagePower, 2) } } else { $null }
        cpu_core_power   = if ($null -ne $cpuCorePower) { @{ available = $true; value = [math]::Round($cpuCorePower, 2) } } else { $null }
        cpu_soc_power    = if ($null -ne $cpuSocPower) { @{ available = $true; value = [math]::Round($cpuSocPower, 2) } } else { $null }
        cpu_misc_power   = if ($null -ne $cpuMiscPower) { @{ available = $true; value = [math]::Round($cpuMiscPower, 2) } } else { $null }
        cpu_clock        = if ($null -ne $cpuClock -and $cpuClock -gt 0) { @{ available = $true; value = [math]::Round($cpuClock, 0) } } else { $null }
        psu_output_power = if ($null -ne $psuOutputPower) { @{ available = $true; value = [math]::Round($psuOutputPower, 2) } } else { $null }
        temperatures     = $temperatures
        library_path     = $dll
    }
    $result | ConvertTo-Json -Compress -Depth 6
}
catch {
    Emit-Error $_.Exception.Message
}
