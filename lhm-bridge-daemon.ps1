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
#   - For each non-empty line read from stdin (content is ignored; any line is a
#     "read" request): Update all hardware + subhardware, build the result
#     object, write exactly one compact JSON line, flush.
#   - On a per-read exception: write one unavailable error object, flush, keep
#     looping. The agent decides whether to recycle.
#   - On stdin EOF (ReadLine returns $null, i.e. the agent closed stdin for a
#     clean shutdown): exit 0.
#
# The agent periodically kills and restarts this process (every N reads / M
# minutes) so hot-plugged hardware (USB PSU) is re-enumerated with a fresh
# Computer.Open(), and to shed any long-lived driver/handle drift.
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
    $computer.IsPowerMonitorEnabled = $true
    if ($EnablePsu -and $computer.PSObject.Properties['IsPsuEnabled']) {
        $computer.IsPsuEnabled = $true
    }
    return $computer
}

function New-ErrorObject([string]$message) {
    return @{ available = $false; error = $message; power = $null; cpu_clock = $null; psu_output_power = $null; temperatures = @() }
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

# One Update + read pass against an already-open Computer. Returns the compact
# JSON string the Go agent parses. Mirrors the sensor-selection logic of the
# one-shot script exactly (same field names, same selection rules) so the JSON
# contract is identical. Unlike the one-shot it does a SINGLE pass per call -
# no 3x retry / settle loop - because the daemon gets a fresh pass every slow
# lane tick, so a stale first reading self-heals on the next sample. This is
# what makes each read sub-second.
function Read-LhmSnapshot($computer) {
    $cpuPackagePower = $null
    $cpuClock = $null
    $psuOutputPower = $null
    $temperatures = New-Object System.Collections.Generic.List[object]

    foreach ($hw in $computer.Hardware) {
        try { $hw.Update() } catch {}
        $sensors = @($hw.Sensors)
        # First CPU package power sensor wins (matches the agent selection rule).
        if (-not $cpuPackagePower -and ($hw.Name -match 'Ryzen|Intel|AMD|EPYC|Xeon|Core')) {
            $pkg = $sensors | Where-Object {
                $_.SensorType -eq 'Power' -and $_.Name -eq 'Package' -and $_.Value -ne $null
            } | Select-Object -First 1
            if ($pkg) { $cpuPackagePower = [double]$pkg.Value }
        }
        # Live CPU clock: the average per-core clock in MHz. LHM reads the
        # per-core MSRs on each Update(), so unlike Win32_Processor's static
        # CurrentClockSpeed this tracks real load. Prefer LHM's own 'Cores
        # (Average)' aggregate; otherwise average the individual 'Core #N'
        # clocks. Per-domain ('Bus Speed', 'Fabric', 'Memory', 'Uncore') and
        # '(Effective)' sensors are excluded - effective clocks collapse toward
        # 0 in idle C-states and would read as a misleading sub-GHz value.
        if ($null -eq $cpuClock -and ($hw.Name -match 'Ryzen|Intel|AMD|EPYC|Xeon|Core')) {
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
        cpu_clock        = if ($null -ne $cpuClock -and $cpuClock -gt 0) { @{ available = $true; value = [math]::Round($cpuClock, 0) } } else { $null }
        psu_output_power = if ($null -ne $psuOutputPower) { @{ available = $true; value = [math]::Round($psuOutputPower, 2) } } else { $null }
        temperatures     = $temperatures
    }
    return $result | ConvertTo-Json -Compress -Depth 6
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
