<#
.SYNOPSIS
  Stop any running sysmon-agent on the target port, rebuild the Go binary,
  and start a fresh agent.

.DESCRIPTION
  Stop any running sysmon-agent on the target port, rebuild the Go binary,
  and start a fresh agent. Use after editing Go sources or lhm-bridge.ps1 - the
  bridge is embedded at build time via go:embed, so a rebuild is required for
  bridge changes to take effect.

.PARAMETER Bind
  Bind address passed to the agent. Defaults to 0.0.0.0.

.PARAMETER Port
  TCP port the agent listens on. Defaults to 9099. Any process currently
  listening on this port is stopped first.

.PARAMETER Background
  Run the agent detached (hidden window, survives terminal close). Without
  this switch the agent runs in the foreground so logs stream to the console
  and Ctrl+C stops it.

.PARAMETER NoBuild
  Skip the `go build` step and reuse the existing sysmon-agent.exe.

.EXAMPLE
  .\run-windows.ps1
  Stop any agent on :9099, rebuild, and run in the foreground.

.EXAMPLE
  .\run-windows.ps1 -Background
  Stop, rebuild, and run detached.

.EXAMPLE
  .\run-windows.ps1 -Port 9100 -NoBuild
  Reuse the current binary and listen on :9100.
#>
param(
    [string]$Bind = '0.0.0.0',
    [ValidateRange(1, 65535)]
    [int]$Port = 9099,
    [switch]$Background,
    [switch]$NoBuild
)

$ErrorActionPreference = 'Stop'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$AgentPath = Join-Path $ScriptDir 'sysmon-agent.exe'

function Stop-AgentOnPort([int]$TargetPort) {
    $owners = Get-NetTCPConnection -LocalPort $TargetPort -State Listen -ErrorAction SilentlyContinue |
        Select-Object -ExpandProperty OwningProcess -Unique
    foreach ($procId in $owners) {
        try {
            $proc = Get-Process -Id $procId -ErrorAction Stop
            Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
            Write-Host "Stopped old agent: $($proc.Name) (PID $procId) on :$TargetPort"
        } catch {
            # Process may have exited between query and stop; ignore.
        }
    }
    if (-not $owners) {
        Write-Host "No agent listening on :$TargetPort."
    }
}

Write-Host "==> Stopping any agent on :$Port"
Stop-AgentOnPort $Port

if ($NoBuild) {
    if (-not (Test-Path $AgentPath)) {
        throw "sysmon-agent.exe not found at $AgentPath (remove -NoBuild to build it first)"
    }
    Write-Host "==> Skipping build (reusing $AgentPath)"
} else {
    Write-Host "==> Building sysmon-agent"
    Push-Location $ScriptDir
    try {
        # The Go module lives in this directory; build in place so go:embed
        # lhm-bridge.ps1 is re-baked into the binary on every rebuild.
        go build -o $AgentPath .
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
    Write-Host "    built: $AgentPath"
}

# Brief pause so the OS releases the port before we rebind.
Start-Sleep -Milliseconds 500

$agentArgs = @('-bind', $Bind, '-port', $Port)

if ($Background) {
    Write-Host "==> Starting agent in background: $AgentPath $($agentArgs -join ' ')"
    Start-Process -FilePath $AgentPath -ArgumentList $agentArgs -WindowStyle Hidden
    Start-Sleep -Seconds 1
    $listeners = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue |
        Select-Object -ExpandProperty OwningProcess -Unique
    if ($listeners) {
        Write-Host "    agent running (PID $($listeners -join ',')) on :$Port"
    } else {
        Write-Warning "Agent started but nothing is listening on :$Port yet."
    }
} else {
    Write-Host "==> Starting agent in foreground (Ctrl+C to stop): $AgentPath $($agentArgs -join ' ')"
    & $AgentPath @agentArgs
}
