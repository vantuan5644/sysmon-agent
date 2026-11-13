package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWindowsInstallerDefaults(t *testing.T) {
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`param(`,
		`[ValidatePattern('^[A-Za-z0-9_.-]+$')]`,
		`[ValidateRange(1, 65535)]`,
		`[ValidateRange(1, 300)]`,
		// Assert the typed readiness-timeout param exists but not its exact default:
		// sysmon-agent/CLAUDE.md lists $ReadinessTimeoutSeconds as a cosmetic value the
		// release and monorepo copies are allowed to diverge on (it is 45 here, was 20).
		`[int]$ReadinessTimeoutSeconds =`,
		// The public installer registers `SysmonAgent`; the monorepo copy uses
		// `HomelabSysmonAgent`. This divergence is intentional and documented --
		// see apps/sysmon-agent/CLAUDE.md. This file is release-owned so the
		// upstream expectation is never synced over it.
		`$ServiceName = 'SysmonAgent'`,
		`$SettingsPath = "$env:ProgramData\SysmonAgent\settings.json"`,
		`-Profile Domain,Private`,
		`Command-line values cannot contain double quotes`,
		`function Resolve-HealthHost`,
		`function Format-UrlHost`,
		`function Test-UsableDeviceHost`,
		`function Test-DeviceInterfaceName`,
		`InterfaceAlias`,
		`Test-DeviceInterfaceName ([string]$_.InterfaceAlias)`,
		`if ($hosts.Count -eq 0)`,
		`'vethernet'`,
		`'wsl'`,
		`function Get-DeviceHostPriority`,
		`function Get-CandidateDeviceHosts`,
		`function Get-DeviceUrls`,
		`function Show-DeviceHandoff`,
		`Get-NetIPAddress`,
		`${scheme}://$(Format-UrlHost $_):$Port/`,
		`Sysmon device URLs:`,
		`Final Sysmon device verification:`,
		`.\verify-deployed-windows.ps1 -HoldSeconds 120`,
		`Add the Sysmon URL to your device Home Screen, then open that Home Screen app and tap the status strip during the hold.`,
		`function Get-ReadinessCheckUrl`,
		`function Get-StatusCheckUrl`,
		`function Get-AgentBaseUrl`,
		`function Wait-AgentReady`,
		`Invoke-RestMethod -Method Get -Uri $readyUrl -TimeoutSec 4`,
		`Readiness check ok: $readyUrl`,
		`did not become ready at $readyUrl within ${ReadinessTimeoutSeconds}s`,
		`function Show-AgentReadiness`,
		`Dashboard readiness ok: $readyUrl`,
		`Dashboard readiness failed at ${readyUrl}`,
		`function Format-OnOff`,
		`function Show-DashboardSettings`,
		`Invoke-RestMethod -Method Get -Uri $statusUrl -TimeoutSec 4`,
		`$dashboardBuild = if ([string]::IsNullOrWhiteSpace([string]$status.dashboard_build)) { 'unknown' } else { [string]$status.dashboard_build }`,
		`Dashboard settings: $persistence, build=$dashboardBuild, refresh=$($settings.refresh_ms)ms, panel=$($settings.panel), dim=$(Format-OnOff $settings.dim), shift=$(Format-OnOff $settings.shift)`,
		`thresholds: CPU $($settings.thresholds.cpu_warn)% / RAM $($settings.thresholds.memory_warn)% / Disk $($settings.thresholds.disk_warn)% / GPU $($settings.thresholds.gpu_warn)% / Temp $($settings.thresholds.temp_warn_c)C`,
		`Dashboard settings unavailable at ${statusUrl}`,
		`Dashboard settings failed at ${statusUrl}`,
		// Claude quota page: the -ClaudeQuotaPoll switch and its Status readout.
		// Code-only needles -- the monorepo copy comment prose differs.
		`[switch]$ClaudeQuotaPoll,`,
		`$binPath += ' -claude-quota-poll'`,
		`[string]$CodexConfigDir =`,
		`$binPath += " -codex-config-dir $(Quote-Arg $CodexConfigDir)"`,
		`[switch]$CodexQuotaPoll,`,
		`[string]$CodexBinary = ''`,
		`-codex-quota-poll -codex-binary $(Quote-Arg $quotaBinary)`,
		`function Get-QuotaCheckUrl`,
		`function Show-QuotaStatus`,
		`Claude quota page: source=`,
		`function Get-CodexUsageCheckUrl`,
		`function Show-CodexUsageStatus`,
		`Codex usage page: source=`,
		`function Test-DeviceClientCheckEvidence`,
		`$mobileDevice = $userAgent.Contains('Mobile') -or $userAgent.Contains('iPhone') -or $userAgent.Contains('iPad') -or $userAgent.Contains('iPod') -or $userAgent.Contains('Android')`,
		`function Test-StandaloneClientCheckEvidence`,
		`function Test-InteractionClientCheckEvidence`,
		`function Get-TimeMilliseconds`,
		`function Get-ClientCheckAgeSeconds`,
		`function Test-ClientCheckStale`,
		`SYSMON_STATUS_CLIENT_FRESH_SECONDS`,
		`function Get-CurrentDashboardBuild`,
		`function Get-ClientCheckRank`,
		`function Get-BestClientCheck`,
		`function Get-StatusClientCheck`,
		`function Get-LatestHomeScreenClientCheck`,
		`function Get-ClientCheckIdentityKey`,
		`function Show-RecentHomeScreenActivity`,
		`$status.device_client_check`,
		`$status.client_check`,
		`function Show-ClientCheckStatus`,
		`$historyUrl = "$baseUrl/api/client-checks"`,
		`$latestUrl = "$baseUrl/api/client-check"`,
		`$statusClientCheck = Get-StatusClientCheck`,
		`$latestHomeScreenClientCheck = Get-LatestHomeScreenClientCheck $entries`,
		`$selectedClientCheck = Get-BestClientCheck @($clientCheck, $statusClientCheck)`,
		`$selectedHomeScreenClientCheck = Get-LatestHomeScreenClientCheck @($latestHomeScreenClientCheck, $statusClientCheck)`,
		`[object]::ReferenceEquals($selectedClientCheck, $statusClientCheck)`,
		`$url = Get-StatusCheckUrl`,
		`device client: not observed yet`,
		`device client: $label Home Screen stale at`,
		`device client: $label Home Screen stale dashboard build at`,
		`device client: $label Home Screen status-strip tap seen at`,
		`Show-RecentHomeScreenActivity $clientCheck $latestHomeScreenClientCheck`,
		`device client: recent Home Screen activity at`,
		`device client: $label Home Screen seen without status-strip tap at`,
		`function Resolve-IntEnv`,
		`SYSMON_DEPLOY_VERIFY_REPORT_FRESH_SECONDS`,
		`function Get-DeployedReportPath`,
		`SYSMON_DEPLOY_VERIFY_REPORT`,
		`sysmon-agent-deployed-verify-report.txt`,
		`function Get-DeployedReportField`,
		`function Get-ReportAgeSeconds`,
		`function Show-DeployedReportUrl`,
		`$deviceUrl = Get-DeployedReportField $ReportPath 'device_url'`,
		`$deviceUrlSource = Get-DeployedReportField $ReportPath 'device_url_source'`,
		`Last device URL: $deviceUrl$sourceLabel`,
		`function Show-DeployedDeviceGate`,
		`$clientSeen = Get-DeployedReportField $reportPath 'device_client_seen'`,
		`$clientInteraction = Get-DeployedReportField $reportPath 'device_client_interaction'`,
		`$dashboardBuild = Get-DeployedReportField $reportPath 'dashboard_build'`,
		`$detailParts += "client=$clientSeen"`,
		`$detailParts += "interaction=$clientInteraction"`,
		`$detailParts += "build=$dashboardBuild"`,
		`Last deployed device gate: no report`,
		`Last deployed device gate: unreadable report`,
		`Last deployed device gate: pass but stale`,
		`Show-DeployedReportUrl $reportPath`,
		`.\verify-deployed-windows.ps1 -HoldSeconds 120`,
		`function Remove-ServiceFirewallRules`,
		`Get-NetFirewallRule -Name "$ServiceName-*"`,
		`$rules | Remove-NetFirewallRule`,
		`Remove-ServiceFirewallRules`,
		`function Set-ServiceRecovery`,
		`sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/5000/restart/30000`,
		`sc.exe failureflag $ServiceName 1`,
		`Set-ServiceRecovery`,
		`Wait-AgentReady`,
		`Show-AgentReadiness`,
		`Show-DashboardSettings`,
		`Show-ClientCheckStatus`,
		`Show-DeployedDeviceGate`,
		`Show-DeviceHandoff`,
		`$serviceInstalled = $false`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("install-windows.ps1 missing %q", needle)
		}
	}
}

func TestWindowsInstallerUpdateAction(t *testing.T) {
	// install-windows.ps1 is release-owned; the monorepo copy is a parity
	// mirror (see sync-to-release.sh). Phase 2 of the auto-update plan adds an
	// -Action Update engine: validate the parity copy carries it so the two
	// repos don't drift.
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`[ValidateSet('Install', 'Uninstall', 'Status', 'Update', 'Pair')]`,
		`[string]$UpdateVersion`,
		`[switch]$Force`,
		`[switch]$DryRun`,
		`'Update' { Update-Agent }`,
		`function Update-Agent`,
		`function Get-InstalledExePath`,
		`function Get-InstalledVersion`,
		`function Get-LatestRelease`,
		`function Find-ReleaseAsset`,
		`function Read-PublishedChecksum`,
		`function Invoke-VerifiedSwap`,
		`function Compare-VersionTag`,
		`Get-FileHash -LiteralPath $downloadExe -Algorithm SHA256`,
		`SHA256SUMS.txt has no entry for sysmon-agent.exe`,
		`Checksum mismatch`,
		`rolled back`,
		`releases/latest`,
	} {
		if !strings.Contains(script, needle) {
			t.Errorf("install-windows.ps1 missing %q", needle)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// The public installer requires pairing by default: the service gets
// -pairing required, Install ends with a PIN, -Action Pair makes another, and
// Status and Update say what the agent runs with. Every flag the installer hands
// the agent must be one the agent defines, or the service would not start.
func TestWindowsInstallerPairing(t *testing.T) {
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, needle := range []string{
		"[ValidateSet('required', 'off')]\n    [string]$Pairing = 'required',",
		`-settings $(Quote-Arg $SettingsPath) -pairing $Pairing"`,
		`[switch]$PauseAtEnd`,
		`'Pair' { Use-ServiceEndpoint; Invoke-AgentPair }`,
		`'Status' { Use-ServiceEndpoint; Show-Status }`,
		`$script:GivenParameters = @($PSBoundParameters.Keys)`,
		`on its More status page (the second dot),`,
		`function Invoke-AgentPair`,
		`function Invoke-AgentCommand`,
		`$pairArguments = @('-pair')`,
		`$pairArguments += @('-pair-url', $serveUrl)`,
		`if (-not [Console]::IsOutputRedirected) {`,
		`$pairArguments += '-pair-qr'`,
		`[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false`,
		`[switch]$SkipPair,`,
		`if ($SkipPair) {`,
		`Invoke-AgentCommand @('-pair-list')`,
		`Start-Process -FilePath 'powershell.exe' -Verb RunAs`,
		`function Show-PairingStatus`,
		`function Show-TailscaleServeAdvice`,
		`function Test-NetworkTls`,
		`$scheme = if (Test-NetworkTls) { 'https' } else { 'http' }`,
		`Show-DeviceHandoff -SkipUrls`,
		`Pair your first device:`,
		`This agent now requires pairing`,
	} {
		if !strings.Contains(script, needle) {
			t.Errorf("install-windows.ps1 missing %q", needle)
		}
	}
	// The PIN is the last thing Install prints, so it is what the setup
	// wizard's log ends on.
	install := script[strings.Index(script, "function Install-Agent"):]
	install = install[:strings.Index(install, "\nfunction ")]
	if !strings.HasSuffix(strings.TrimSpace(install[:strings.LastIndex(install, "Invoke-AgentPair")]), "try {") {
		t.Errorf("Install-Agent does not end with the pairing PIN")
	}

	mainGo, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"pairing", "pair", "pair-list", "pair-qr", "pair-url", "network-tls", "settings", "bind", "port"} {
		if !regexp.MustCompile(`flag\.[A-Za-z]+\("` + regexp.QuoteMeta(flag) + `"`).Match(mainGo) {
			t.Errorf("install-windows.ps1 passes -%s, which main.go does not define", flag)
		}
	}

	nsis, err := os.ReadFile("dist/installer.nsi")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`Pair a device.lnk`,
		`-Action Pair -PauseAtEnd`,
		// The PIN shows in a console after Finish, with its QR code, so the
		// install itself opens no pairing window.
		`-Action Install -SkipPair -KeepServiceArguments$6'`,
		`!define MUI_FINISHPAGE_RUN_TEXT "Pair a device now"`,
		`!define MUI_FINISHPAGE_RUN_FUNCTION PairDeviceNow`,
		`Function PairDeviceNow`,
		`Page custom GpuGuardPage GpuGuardPageLeave`,
		`StrCpy $6 " -GpuGuard $GpuGuardMode"`,
		`StrCpy $6 "$6 -GpuHotspotWarn $GpuHotspotWarn"`,
		`StrCpy $6 "$6 -GpuHotspotCrit $GpuHotspotCrit"`,
		`StrCpy $6 "$6 -GpuDeltaWarn $GpuDeltaWarn"`,
		`!insertmacro ReadServiceFlag "-gpu-guard " " " $GpuGuardMode`,
		`!insertmacro ReadSwitch $R0 "/GPUGUARD=" $GpuGuardMode`,
	} {
		if !strings.Contains(string(nsis), needle) {
			t.Errorf("dist/installer.nsi missing %q", needle)
		}
	}
	if strings.Contains(string(nsis), "MUI_FINISHPAGE_NOAUTOCLOSE") {
		t.Errorf("dist/installer.nsi holds the install log open, but the PIN is no longer in it")
	}

	// The wizard checks the guard's lines against the ranges the installer
	// script accepts, and only skips a value equal to the agent's default.
	for _, line := range []struct {
		param             string
		low, high, normal int
	}{
		{"GpuHotspotWarn", 70, 105, 90},
		{"GpuHotspotCrit", 80, 110, 95},
		{"GpuDeltaWarn", 10, 40, 20},
	} {
		ps1 := fmt.Sprintf("[ValidateScript({ $_ -eq 0 -or ($_ -ge %d -and $_ -le %d) })]\n    [int]$%s = 0,", line.low, line.high, line.param)
		if !strings.Contains(script, ps1) {
			t.Errorf("install-windows.ps1 no longer takes -%s as %d-%d", line.param, line.low, line.high)
		}
		if !strings.Contains(string(nsis), fmt.Sprintf("!insertmacro GpuLineInRange $%s %d %d ", line.param, line.low, line.high)) {
			t.Errorf("dist/installer.nsi does not check %s against %d-%d", line.param, line.low, line.high)
		}
		if !strings.Contains(string(nsis), fmt.Sprintf("${If} $%s != %d\n", line.param, line.normal)) {
			t.Errorf("dist/installer.nsi does not treat %d as the default %s", line.normal, line.param)
		}
	}
}

// Pair and Status read the service's own command line, which sc.exe stores
// with each value quoted the way Get-BinaryPath wrote it.
func TestWindowsInstallerReadsServiceArguments(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell 7 is needed to run the installer's functions")
	}
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	functions := sliceFunction(t, source, "Get-ServiceArgument") + "\n}\n" + sliceFunction(t, source, "Get-ServicePairingMode") + "\n}\n"
	script := `$ErrorActionPreference = 'Stop'
` + functions + `
$line = '"C:\Program Files\Sysmon Agent\sysmon-agent.exe" -bind "0.0.0.0" -port 9199 -settings "C:\ProgramData\Sysmon Agent Two\settings.json" -pairing required -gpu-guard off'
if ((Get-ServiceArgument $line 'settings') -ne 'C:\ProgramData\Sysmon Agent Two\settings.json') { throw 'settings' }
if ((Get-ServiceArgument $line 'bind') -ne '0.0.0.0') { throw 'bind' }
if ((Get-ServiceArgument $line 'port') -ne '9199') { throw 'port' }
if ((Get-ServiceArgument $line 'pair') -ne $null) { throw '-pair matched -pairing' }
if ((Get-ServicePairingMode $line) -ne 'required') { throw 'mode' }
$old = '"C:\Program Files\Sysmon Agent\sysmon-agent.exe" -bind "0.0.0.0" -port 9099 -settings "C:\ProgramData\SysmonAgent\settings.json"'
if ((Get-ServicePairingMode $old) -ne 'required') { throw 'an install from before pairing runs the agent default' }
if ((Get-ServicePairingMode ($old + ' -pairing off')) -ne 'off') { throw 'off' }
if ((Get-ServiceArgument '' 'settings') -ne $null) { throw 'empty' }
`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell fixtures: %v: %s", err, out)
	}
}

// The setup wizard upgrades with -KeepServiceArguments: whatever it does not
// name keeps the installed service's value, so an upgrade leaves the port, the
// settings folder and the flags set earlier with the script alone. The wizard
// runs Windows PowerShell 5.1, so this runs there too when it is installed.
func TestWindowsInstallerKeepsServiceArguments(t *testing.T) {
	shells := []string{}
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			shells = append(shells, path)
		}
	}
	if len(shells) == 0 {
		t.Skip("PowerShell is needed to run the installer's functions")
	}
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	start := strings.Index(source, "param(\n")
	end := strings.Index(source[start:], "\n)\n")
	if start < 0 || end < 0 {
		t.Fatal("install-windows.ps1: no param block")
	}
	params := source[start : start+end+3]
	functions := sliceFunction(t, source, "Get-ServiceArgument") + "\n}\n" + sliceFunction(t, source, "Use-ServiceArguments") + "\n}\n"
	script := "[CmdletBinding()]\n" + params + `$ErrorActionPreference = 'Stop'
$WarningPreference = 'SilentlyContinue'
` + functions + `
function Get-ServiceCommandLine { return $script:CommandLine }
function Reset {
    $script:Port = 9099; $script:Bind = '0.0.0.0'; $script:Pairing = 'required'
    $script:SettingsPath = 'C:\ProgramData\SysmonAgent\settings.json'
    $script:ClaudeConfigDir = 'C:\Users\me\.claude'; $script:CodexConfigDir = 'C:\Users\me\.codex'
    $script:ClaudeQuotaPoll = [switch]$false; $script:CodexQuotaPoll = [switch]$false
    $script:CodexBinary = ''; $script:TempWarn = 0; $script:GpuGuard = ''
    $script:GpuHotspotWarn = 0; $script:GpuHotspotCrit = 0; $script:GpuDeltaWarn = 0; $script:GpuGuardSpare = ''
}
function Check($what, $got, $want) { if ($got -ne $want) { throw "${what}: got '$got', want '$want'" } }

# What the wizard names wins; everything else comes from the service.
Reset
$script:CommandLine = '"C:\Program Files\Sysmon Agent\sysmon-agent.exe" -bind "127.0.0.1" -port 9199 -settings "C:\ProgramData\Sysmon Agent Two\settings.json" -pairing required -claude-config-dir "C:\Users\me\.claude" -claude-quota-poll -temp-warn 85 -gpu-guard off -gpu-hotspot-crit 93 -gpu-guard-spare "Code.exe"'
$script:GivenParameters = @('Action', 'SkipPair', 'KeepServiceArguments', 'GpuGuard', 'GpuHotspotWarn', 'GpuHotspotCrit', 'GpuDeltaWarn')
$script:GpuGuard = 'act'
Use-ServiceArguments
Check port $Port 9199
Check bind $Bind '127.0.0.1'
Check settings $SettingsPath 'C:\ProgramData\Sysmon Agent Two\settings.json'
Check claude $ClaudeConfigDir 'C:\Users\me\.claude'
Check 'claude poll' ([bool]$ClaudeQuotaPoll) $true
Check 'codex dir, missing from the line' $CodexConfigDir ''
Check 'codex poll' ([bool]$CodexQuotaPoll) $false
Check 'temp warn' $TempWarn 85
Check 'named guard' $GpuGuard 'act'
Check 'named crit' $GpuHotspotCrit 0
Check spare $GpuGuardSpare 'Code.exe'
if ($Port -isnot [int]) { throw 'port lost its type' }

# A value the parameter refuses stays at the default, with a warning.
Reset
$script:CommandLine = '"C:\x\sysmon-agent.exe" -bind "0.0.0.0" -port 99999 -settings "C:\s.json" -pairing maybe -gpu-guard loud'
$script:GivenParameters = @('Action')
Use-ServiceArguments
Check 'port out of range' $Port 9099
Check 'unknown pairing' $Pairing 'required'
Check 'unknown guard' $GpuGuard ''
Check settings $SettingsPath 'C:\s.json'

# A line from before pairing has no -pairing: the default stays.
Reset
$script:CommandLine = '"C:\x\sysmon-agent.exe" -bind "0.0.0.0" -port 9099 -settings "C:\s.json"'
Use-ServiceArguments
Check 'old line pairing' $Pairing 'required'

# No service yet: nothing changes.
Reset
$script:CommandLine = $null
Use-ServiceArguments
Check 'fresh claude' $ClaudeConfigDir 'C:\Users\me\.claude'
Check 'fresh port' $Port 9099
'ok'
`
	file := filepath.Join(t.TempDir(), "keep.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, shell := range shells {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
		// The param block's defaults read it, and Linux has none.
		cmd.Env = append(os.Environ(), "USERPROFILE="+t.TempDir())
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || !strings.Contains(string(out), "ok") {
			t.Fatalf("%s: %v: %s", shell, err, out)
		}
	}
}

// Windows PowerShell 5.1 splits a native argument that holds quotes at its
// spaces, so sc.exe config refused the wizard's binPath= and the service kept
// its old command line. The wizard also has to start the 64-bit PowerShell
// and keep what it does not name.
func TestWindowsInstallerChangesServiceWithoutScExe(t *testing.T) {
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.Contains(script, "binPath=") {
		t.Error("install-windows.ps1 still passes binPath= to sc.exe")
	}
	if !strings.Contains(script, "Invoke-CimMethod -MethodName Change -Arguments @{ PathName = $binaryPath;") {
		t.Error("install-windows.ps1 does not change the service through Win32_Service.Change")
	}
	nsi, err := os.ReadFile(filepath.Join("dist", "installer.nsi"))
	if err != nil {
		t.Fatal(err)
	}
	wizard := strings.ReplaceAll(string(nsi), "\r\n", "\n")
	for _, needle := range []string{
		"  ${DisableX64FSRedirection}\n  nsExec::ExecToLog '\"$SYSDIR\\WindowsPowerShell\\v1.0\\powershell.exe\" -NoProfile -ExecutionPolicy Bypass -File \"$INSTDIR\\install-windows.ps1\" -Action Install -SkipPair -KeepServiceArguments$6'",
		"  StrCpy $6 \" -GpuGuard $GpuGuardMode\"",
		"\"URL\" \"http://localhost:$ServicePort/\"",
	} {
		if !strings.Contains(wizard, needle) {
			t.Errorf("installer.nsi lacks %q", needle)
		}
	}
	if strings.Contains(wizard, "nsExec::ExecToLog 'powershell.exe") {
		t.Error("installer.nsi still starts the 32-bit powershell.exe")
	}
}

// Status and Pair run without -Port on a host whose service uses another one,
// such as a second agent beside the first. They must read that service, and
// list its own Tailscale Serve address rather than the agent's on 9099, while
// a -Port the caller names still wins.
func TestWindowsInstallerUsesServiceEndpoint(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell 7 is needed to run the installer's functions")
	}
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	functions := sliceFunction(t, source, "Get-ServiceArgument") + "\n}\n" + sliceFunction(t, source, "Use-ServiceEndpoint") + "\n}\n"
	script := `$ErrorActionPreference = 'Stop'
` + functions + `
$script:CommandLine = '"C:\Program Files\Sysmon Agent\sysmon-agent.exe" -bind "127.0.0.1" -port 9199 -settings "C:\ProgramData\SysmonAgentPublic\settings.json"'
function Get-ServiceCommandLine { return $script:CommandLine }

$script:Port = 9099; $script:Bind = '0.0.0.0'; $script:GivenParameters = @('Action')
Use-ServiceEndpoint
if ($Port -ne 9199) { throw "port: $Port" }
if ($Bind -ne '127.0.0.1') { throw "bind: $Bind" }

$script:Port = 9300; $script:Bind = '0.0.0.0'; $script:GivenParameters = @('Action', 'Port', 'Bind')
Use-ServiceEndpoint
if ($Port -ne 9300) { throw "a named -Port lost: $Port" }
if ($Bind -ne '0.0.0.0') { throw "a named -Bind lost: $Bind" }

$script:CommandLine = $null
$script:Port = 9099; $script:Bind = '0.0.0.0'; $script:GivenParameters = @()
Use-ServiceEndpoint
if ($Port -ne 9099 -or $Bind -ne '0.0.0.0') { throw 'no service changed the defaults' }
`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell fixtures: %v: %s", err, out)
	}
}

// -TailscaleServe publishes the agent at https://<machine>.<tailnet>.ts.net:9443,
// the address shape every host shares and the one peer discovery tries first.
// tailscaled drops a Serve config on logout or reinstall, so Install must
// re-apply it and Status must say when it is missing. Same parity rule as the
// Update engine above.
func TestWindowsInstallerTailscaleServe(t *testing.T) {
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`[switch]$TailscaleServe,`,
		`[int]$TailscaleServePort = 9443,`,
		`function Find-TailscaleCli`,
		`Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'`,
		`function Get-TailscaleServeTarget`,
		`return "http://$(Format-UrlHost (Resolve-HealthHost)):$Port"`,
		`@('serve', '--bg', "--https=$TailscaleServePort", $target)`,
		`@('serve', "--https=$TailscaleServePort", 'off')`,
		`& $TailscaleCli serve status --json 2>$null`,
		`if ($TailscaleServe) {
        Publish-TailscaleServe
    }`,
		`if ($TailscaleServe) {
        Unpublish-TailscaleServe
    }`,
		`Show-AgentReadiness
        Show-TailscaleServeStatus`,
		`Fix: re-run Install with -TailscaleServe.`,
	} {
		// The working tree is CRLF for .ps1 files; compare line-ending blind.
		if !strings.Contains(strings.ReplaceAll(script, "\r\n", "\n"), needle) {
			t.Errorf("install-windows.ps1 missing %q", needle)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// The GPU guard parameters are hand-ported into this release-owned installer,
// so nothing else notices when the agent renames a flag, moves a range or
// changes a default. Each check below reads the agent source the sync brings in.
func TestWindowsInstallerGPUGuard(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// The working tree is CRLF for .ps1 files; compare line-ending blind.
		return strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	script := read("install-windows.ps1")
	agentMain := read("main.go")
	guard := read("internal/gpuguard/guard.go")
	metrics := read("metrics.go")

	for _, needle := range []string{
		"[ValidateSet('', 'off', 'warn', 'act')]\n    [string]$GpuGuard = '',",
		`[string]$GpuGuardSpare = ''`,
		`$binPath += " -gpu-guard $GpuGuard"`,
		`$binPath += " -gpu-hotspot-warn $GpuHotspotWarn"`,
		`$binPath += " -gpu-hotspot-crit $GpuHotspotCrit"`,
		`$binPath += " -gpu-delta-warn $GpuDeltaWarn"`,
		`$binPath += " -gpu-guard-spare $(Quote-Arg $GpuGuardSpare)"`,
		`must be below -GpuHotspotCrit ($effectiveCrit).`,
		`function Show-GpuGuardStatus`,
		"Show-DashboardSettings\n        Show-PairingStatus\n        Show-GpuGuardStatus\n",
	} {
		if !strings.Contains(script, needle) {
			t.Errorf("install-windows.ps1 missing %q", needle)
		}
	}

	// Every -gpu-* flag the installer passes is one the agent defines.
	passed := regexp.MustCompile(`\$binPath \+= " -(gpu-[a-z-]+) `).FindAllStringSubmatch(script, -1)
	if len(passed) != 5 {
		t.Errorf("found %d -gpu-* flags in Get-BinaryPath, want 5", len(passed))
	}
	for _, m := range passed {
		if !regexp.MustCompile(`flag\.\w+\("` + regexp.QuoteMeta(m[1]) + `"`).MatchString(agentMain) {
			t.Errorf("install-windows.ps1 passes -%s, which main.go does not define", m[1])
		}
	}

	// Ranges and defaults match internal/gpuguard.
	constants := func(pattern string) []string {
		t.Helper()
		m := regexp.MustCompile(pattern).FindStringSubmatch(guard)
		if m == nil {
			t.Fatalf("internal/gpuguard/guard.go has no match for %s", pattern)
		}
		return m[1:]
	}
	for _, c := range []struct{ param, pair, def string }{
		{"GpuHotspotWarn", "HotspotWarnC", "DefaultHotspotWarnC"},
		{"GpuHotspotCrit", "HotspotCritC", "DefaultHotspotCritC"},
		{"GpuDeltaWarn", "DeltaWarnC", "DefaultDeltaWarnC"},
	} {
		r := constants(`Min` + c.pair + `, Max` + c.pair + `\s*=\s*(\d+), (\d+)`)
		want := "($_ -ge " + r[0] + " -and $_ -le " + r[1] + ") })]\n    [int]$" + c.param + " = 0,"
		if !strings.Contains(script, want) {
			t.Errorf("install-windows.ps1 range for -%s should be %s-%s (missing %q)", c.param, r[0], r[1], want)
		}
		def := constants(`\b` + c.def + `\s*=\s*(\d+)`)[0]
		if c.param != "GpuDeltaWarn" {
			effective := map[string]string{"GpuHotspotWarn": "$effectiveWarn", "GpuHotspotCrit": "$effectiveCrit"}[c.param]
			want := effective + " = if ($" + c.param + " -ne 0) { $" + c.param + " } else { " + def + " }"
			if !strings.Contains(script, want) {
				t.Errorf("install-windows.ps1 should assume the agent's default %s for -%s (missing %q)", def, c.param, want)
			}
		}
	}
	defaults := constants(`DefaultHotspotWarnC\s*=\s*(\d+)`)[0] + ", " +
		constants(`DefaultHotspotCritC\s*=\s*(\d+)`)[0] + " and " +
		constants(`DefaultDeltaWarnC\s*=\s*(\d+)`)[0]
	if !strings.Contains(script, "(0 keeps the agent's default: "+defaults+")") {
		t.Errorf("install-windows.ps1 parameter comment should list the agent's defaults %s", defaults)
	}

	// The Status readout reads these JSON fields from /api/metrics.
	if !strings.Contains(metrics, `json:"gpu_guard,`) {
		t.Error("metrics.go no longer publishes gpu_guard; recheck Show-GpuGuardStatus")
	}
	for _, field := range []string{"mode", "level", "source", "hotspot_celsius", "core_celsius", "thresholds", "warn_c", "critical_c", "delta_warn_c", "action", "action_error", "error"} {
		if !regexp.MustCompile(`json:"` + field + `[,"]`).MatchString(guard) {
			t.Errorf("internal/gpuguard/guard.go has no %q field; recheck Show-GpuGuardStatus", field)
		}
	}
}

// Windows starts every new network (a new SSID, a first cable) as Public,
// where the Domain,Private rule does not apply, so LAN devices time out while
// the service and every check run on the machine itself look healthy.
// -LanSubnet adds a Public rule limited to the LAN, a plain reinstall keeps it,
// and Status names a Public network that still blocks the port.
func TestWindowsInstallerLanSubnet(t *testing.T) {
	data, err := os.ReadFile("install-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, needle := range []string{
		`[string]$LanSubnet = '',`,
		`$LanSubnetGiven = $PSBoundParameters.ContainsKey('LanSubnet')`,
		`return "$(Get-FirewallRuleName)-lan"`,
		`function Resolve-LanSubnet`,
		`($rule | Get-NetFirewallAddressFilter).RemoteAddress`,
		`function Set-ServiceFirewallRules`,
		// The LAN rule is read before Remove-ServiceFirewallRules deletes it.
		`    $lanAddresses = @(Resolve-LanSubnet)
    Remove-ServiceFirewallRules`,
		`-Profile Domain,Private | Out-Null`,
		`-Profile Public `,
		`-RemoteAddress $lanAddresses | Out-Null`,
		`    try {
        Set-ServiceFirewallRules
    } catch {`,
		`function Get-PublicLanAddresses`,
		`function Get-PublicPortRules`,
		`-PolicyStore ActiveStore`,
		`function Show-LanFirewallStatus`,
		`Fix: re-run Install with -LanSubnet $subnets`,
		// Every Install path warns about a Public network: the two that end on
		// the handoff, and Invoke-AgentPair before the PIN and its QR code,
		// which is also the Pair window the setup wizard opens.
		`    if ($Pairing -ne 'required') {
        Show-LanFirewallStatus
        Show-DeviceHandoff`,
		`    if ($SkipPair) {
        Show-LanFirewallStatus
        Show-DeviceHandoff`,
		`        Show-TailscaleServeAdvice
        # A phone on a Public network times out on the code's address, while
        # everything checked from this machine passes.
        Show-LanFirewallStatus
`,
		`    if ($serviceInstalled) {
        Show-LanFirewallStatus`,
	} {
		if !strings.Contains(script, needle) {
			t.Errorf("install-windows.ps1 missing %q", needle)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell 7 is needed to run the subnet fixtures")
	}
	validate := regexp.MustCompile(`(?m)^    \[ValidateScript\(\{ (.*) \}\)\]\n    \[string\]\$LanSubnet`).FindStringSubmatch(script)
	if validate == nil {
		t.Fatal("-LanSubnet ValidateScript not found")
	}
	fixture := `$ErrorActionPreference = 'Stop'
` + sliceFunction(t, script, "Get-Ipv4Subnet") + "\n}\n" + `
$cases = @(
    @('10.0.0.4', 24, '10.0.0.0/24'),
    @('192.168.1.77', 24, '192.168.1.0/24'),
    @('172.20.13.9', 20, '172.20.0.0/20'),
    @('10.250.7.9', 9, '10.128.0.0/9'),
    @('100.64.12.34', 32, '100.64.12.34/32')
)
foreach ($c in $cases) {
    $got = Get-Ipv4Subnet $c[0] $c[1]
    if ($got -ne $c[2]) { throw "Get-Ipv4Subnet $($c[0]) $($c[1]) = $got, want $($c[2])" }
}
function Test-LanSubnetValue([string]$Value) { return [bool]($Value | ForEach-Object { ` + validate[1] + ` }) }
foreach ($ok in @('', '10.0.0.0/24', '10.0.0.0/24, 192.168.50.0/24', '10.0.0.0/255.255.255.0')) {
    if (-not (Test-LanSubnetValue $ok)) { throw "-LanSubnet '$ok' rejected" }
}
foreach ($bad in @('Any', 'any', '*', '0.0.0.0/0', '::/0', '10.0.0.0/24, Any', ' * ')) {
    if (Test-LanSubnetValue $bad) { throw "-LanSubnet '$bad' accepted; it opens the port to every address" }
}
`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", fixture).CombinedOutput()
	if err != nil {
		t.Fatalf("LanSubnet fixtures: %v: %s", err, out)
	}
}

func TestWindowsVerifierDefaults(t *testing.T) {
	data, err := os.ReadFile("verify-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`param(`,
		`$Bind = '127.0.0.1'`,
		`$CheckHost = ''`,
		`$DeviceUrl = ''`,
		`[switch]$RequireDevice`,
		`[ValidateRange(1, 65535)]`,
		`[ValidateRange(0, 86400)]`,
		`$HoldSeconds = 0`,
		`$AgentPath = Join-Path $TempRoot 'sysmon-agent.exe'`,
		`$ReportPath = if ($env:SYSMON_VERIFY_REPORT)`,
		`sysmon-agent-verify-report.txt`,
		`configured_device_url=$DeviceUrl`,
		`require_device=$($RequireDevice.IsPresent.ToString().ToLowerInvariant())`,
		`installed_device_home_screen=not_verified`,
		`function Write-Report`,
		`function Write-ClientCheckReport`,
		`function Write-ReadinessCollectionErrors`,
		`${Prefix}_collection_errors=none`,
		`${Prefix}_collection_error_$($i + 1)=$($errors[$i])`,
		`'screen_width', 'screen_height'`,
		`'touch_points'`,
		`'display_mode'`,
		`'orientation'`,
		`function Test-DeviceClientCheckEvidence`,
		`$mobileDevice = $userAgent.Contains('Mobile') -or $userAgent.Contains('iPhone') -or $userAgent.Contains('iPad') -or $userAgent.Contains('iPod') -or $userAgent.Contains('Android')`,
		`$userAgent.Contains('Mobile')`,
		`function Get-TimeMilliseconds`,
		`function Test-FreshClientCheck`,
		`(Test-FreshClientCheck $clientCheckAfter) -and $null -eq $seenClientCheck`,
		`$HoldStartedMs = $null`,
		`$env:GOCACHE = Join-Path ([System.IO.Path]::GetTempPath()) 'go-build-cache'`,
		`go build -o $AgentPath .`,
		`Get-Command node`,
		`node verify-dashboard.mjs`,
		`node verify-api.mjs --settings-roundtrip $BaseUrl`,
		`node verify-render.mjs`,
		`'"dashboard_build"'`,
		`Write-Report 'api_schema=pass'`,
		`Write-Report 'dashboard_render=pass'`,
		`Write-Report 'dashboard_render=skipped_browser_unavailable'`,
		`Write-Report 'dashboard_assets=pass'`,
		`function Invoke-Headers`,
		`function Assert-HeaderContains`,
		`Assert-HeaderContains "$BaseUrl/api/status" 'Cache-Control' 'no-store'`,
		`Assert-HeaderContains "$BaseUrl/api/status" 'Content-Security-Policy' "default-src 'self'"`,
		`Assert-HeaderContains "$BaseUrl/" 'Cache-Control' 'no-cache'`,
		`Assert-HeaderContains "$BaseUrl/" 'X-Content-Type-Options' 'nosniff'`,
		`function Resolve-CheckHost`,
		`function Format-UrlHost`,
		`function Test-DeviceInterfaceName`,
		`InterfaceAlias`,
		`Test-DeviceInterfaceName ([string]$_.InterfaceAlias)`,
		`if ($addresses.Count -eq 0)`,
		`'vethernet'`,
		`'wsl'`,
		`function Get-DeviceHostPriority`,
		`$bytes[0] -eq 100 -and $bytes[1] -ge 64 -and $bytes[1] -le 127`,
		`($bytes[0] -band 0xfe) -eq 0xfc`,
		`function Resolve-DeviceHosts`,
		`Get-NetIPAddress`,
		`Sort-Object @{ Expression = { Get-DeviceHostPriority $_ }; Ascending = $true }`,
		`function Write-DeviceUrls`,
		`function Get-DeviceUrls`,
		`http://$(Format-UrlHost $hostName):$Port/`,
		`Write-Host "  $DeviceUrl"`,
		`Write-Report "device_url=$DeviceUrl"`,
		`Write-Report "device_url=$url"`,
		`For a device check, rerun with -Bind 0.0.0.0 -HoldSeconds 120 -RequireDevice.`,
		`Verification report: $ReportPath`,
		`Write-Report 'result=pass'`,
		`Starting sysmon-agent on ${Bind}:$Port; checking $BaseUrl`,
		`Write-Report 'agent_spawned=pass'`,
		`Write-Report 'agent_ready=fail'`,
		`Write-Report 'agent_ready=pass'`,
		`$script:HoldStartedMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()`,
		`Write-Report "hold_started_ms=$script:HoldStartedMs"`,
		`Start-Sleep -Seconds $HoldSeconds`,
		`/api/metrics`,
		`/readyz`,
		`/api/status`,
		`/api/settings`,
		`/api/client-check`,
		`/api/client-checks`,
		`Checking /api/client-check`,
		`Write-Report 'client_check=pass'`,
		`Write-Report 'readyz=pass'`,
		`Write-ReadinessCollectionErrors 'readyz'`,
		`function Get-ClientCheckEntries`,
		`function Get-StatusClientCheckEntries`,
		`$status.device_client_check`,
		`$status.client_check`,
		`$entries += Get-StatusClientCheckEntries $Url`,
		`foreach ($clientCheckAfter in @(Get-ClientCheckEntries $BaseUrl))`,
		`Test-FreshClientCheck $clientCheckAfter`,
		`Write-Report 'device_client_seen=pass'`,
		`Write-ClientCheckReport $deviceClientCheck`,
		`Write-ClientCheckReport $staleDeviceClientCheck`,
		`Write-Report 'device_client_seen=stale_client'`,
		`Write-ClientCheckReport $seenClientCheck`,
		`Write-Report 'device_client_seen=unexpected_client'`,
		`required device client check was not observed`,
		`Write-Report 'device_client_seen=not_observed'`,
		`Write-Report 'device_client_seen=not_attempted_loopback_bind'`,
		`required device client check cannot run on a loopback-only bind`,
		`Write-Report 'device_client_seen=not_attempted'`,
		`required device client check needs -HoldSeconds greater than zero`,
		`Checking live API schema`,
		`live API schema validation failed`,
		`Checking settings readback and persistence`,
		`-Headers @{ Origin = $BaseUrl }`,
		`$settingsReadback = Invoke-RestMethod`,
		`-Method Get`,
		`"shift":\s*true`,
		`settings file was not written`,
		`"panel":\s*"gpu"`,
		`"cpu_warn":\s*80`,
		`"temp_warn_c":\s*75`,
		`unexpected threshold settings response`,
		`unexpected threshold settings readback`,
		`fetchMetrics`,
		`isLiveEndpoint`,
		`sysmon-agent Windows smoke test passed`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-windows.ps1 missing %q", needle)
		}
	}
}

func TestWindowsDeployedVerifierDefaults(t *testing.T) {
	data, err := os.ReadFile("verify-deployed-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`param(`,
		`$BaseUrl = ''`,
		`$DeviceUrl = ''`,
		`[ValidateRange(0, 86400)]`,
		`$HoldSeconds = 120`,
		`$RequireDevice = $true`,
		`$RequireStandalone = $true`,
		`SYSMON_DEPLOY_VERIFY_BASE_URL`,
		`SYSMON_DEPLOY_VERIFY_DEVICE_URL`,
		`SYSMON_DEPLOY_VERIFY_REQUIRE_STANDALONE`,
		`sysmon-agent-deployed-verify-report.txt`,
		`function Test-PwaInstallAssets`,
		`function Test-PngDimensions`,
		`function Test-SettingsSameOriginRoundTrip`,
		`function Test-ClientCheckSameOriginRoundTrip`,
		`-Headers (@{ Origin = $Url } + $script:AuthHeaders)`,
		`$PSDefaultParameterValues['Invoke-RestMethod:Headers'] = $script:AuthHeaders`,
		`Write-Report 'public_pairing_required=pass'`,
		`thresholds = [ordered]@{`,
		`settings thresholds.$key roundtrip mismatch`,
		`$statusAfterSettings = Invoke-RestMethod -Method Get -Uri "$Url/api/status" -TimeoutSec 5`,
		`$statusSettings = $statusAfterSettings.settings`,
		`settings status.$key roundtrip mismatch`,
		`settings status.thresholds.$key roundtrip mismatch`,
		`Checking published interactive settings controls`,
		`Write-Report 'public_settings_roundtrip=pass'`,
		`Checking published client-check controls`,
		`dashboard_build = $script:ExpectedDashboardBuild`,
		`foreach ($key in @('dashboard_build',`,
		`$statusAfterClientCheck = Invoke-RestMethod -Method Get -Uri "$Url/api/status" -TimeoutSec 5`,
		`$statusClientCheck = $statusAfterClientCheck.client_check`,
		`status client_check $key roundtrip mismatch`,
		`Write-Report 'public_client_check_roundtrip=pass'`,
		`<link rel="manifest" href="/manifest.json">`,
		`<link rel="apple-touch-icon" sizes="180x180" href="/icon-180.png">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">`,
		`<meta name="apple-mobile-web-app-title" content="Sysmon">`,
		`'"id"\s*:\s*"/"'`,
		`'"display"\s*:\s*"standalone"'`,
		`'"background_color"\s*:\s*"#080b10"'`,
		`'"theme_color"\s*:\s*"#080b10"'`,
		`'"src"\s*:\s*"/icon-512.png"'`,
		`Test-PngDimensions "$Url/icon-180.png" 180`,
		`Test-PngDimensions "$Url/icon-512.png" 512`,
		`'display_mode'`,
		`function Test-DeviceClientCheckEvidence`,
		`$mobileDevice = $userAgent.Contains('Mobile') -or $userAgent.Contains('iPhone') -or $userAgent.Contains('iPad') -or $userAgent.Contains('iPod') -or $userAgent.Contains('Android')`,
		`function Test-StandaloneClientCheckEvidence`,
		`[string]$ClientCheck.display_mode -eq 'standalone'`,
		`function Test-InteractionClientCheckEvidence`,
		`[string]$ClientCheck.interaction`,
		`[bool]$RequireInteraction = $true`,
		`SYSMON_DEPLOY_VERIFY_REQUIRE_INTERACTION`,
		`require_interaction=$($RequireInteraction.ToString().ToLowerInvariant())`,
		`function Test-CurrentDashboardBuildEvidence`,
		`[string]$ClientCheck.dashboard_build -eq $script:ExpectedDashboardBuild`,
		`function Test-FreshClientCheck`,
		`function Write-ReadinessCollectionErrors`,
		`${Prefix}_collection_errors=none`,
		`${Prefix}_collection_error_$($i + 1)=$($errors[$i])`,
		`function Write-ClientCheckResult`,
		`if (-not (Test-FreshClientCheck $ClientCheck))`,
		`function Get-ClientCheckEntries`,
		`/api/client-checks`,
		`function Get-StatusClientCheckEntries`,
		`$status.device_client_check`,
		`$status.client_check`,
		`$entries += Get-StatusClientCheckEntries $BaseUrl`,
		`function Get-LatestSeenClientCheck`,
		`[bool]$DeviceOnly = $false`,
		`if ($DeviceOnly -and -not (Test-DeviceClientCheckEvidence $clientCheck))`,
		`Get-LatestSeenClientCheck -ClientChecks (Get-ClientCheckEntries $BaseUrl) -DeviceOnly $true`,
		`function Wait-ForClientCheckEvidence`,
		`(Test-FreshClientCheck $clientCheck) -and $null -eq $pollSeenClientCheck`,
		`Start-Sleep -Seconds ([Math]::Min`,
		`Wait-ForClientCheckEvidence $BaseUrl $HoldSeconds $RequireDevice $RequireStandalone $RequireInteraction`,
		`$script:HoldStartedMs = $null`,
		`hold_started_ms=$script:HoldStartedMs`,
		`function Get-TailscaleHostName`,
		`tailscale status --self --json`,
		`function Get-DirectLanDeviceUrl`,
		`function Get-CandidateDeviceHosts`,
		`function Get-CandidatePriority`,
		`function Test-UsableDeviceHost`,
		`function Test-DeviceInterfaceName`,
		`InterfaceAlias`,
		`Test-DeviceInterfaceName ([string]$_.InterfaceAlias)`,
		`if ($hosts.Count -eq 0)`,
		`'vethernet'`,
		`'wsl'`,
		`Get-DirectLanDeviceUrl $BaseUrl`,
		`device_url_source=$script:DeviceUrlSource`,
		`node verify-api.mjs $BaseUrl`,
		`node verify-api.mjs $phoneUrl`,
		`Write-Report 'readyz=pass'`,
		`Write-Report 'public_readyz=pass'`,
		`Write-ReadinessCollectionErrors 'readyz' $ready`,
		`Write-ReadinessCollectionErrors 'public_readyz' $publicReady`,
		`Write-Report 'public_api_schema=pass'`,
		`$script:ExpectedDashboardBuild = ''`,
		`$publicStatus = Invoke-RestMethod -Method Get -Uri "$phoneUrl/api/status" -TimeoutSec 5`,
		`$script:ExpectedDashboardBuild = [string]$publicStatus.dashboard_build`,
		`Write-Report "dashboard_build=$script:ExpectedDashboardBuild"`,
		`Write-Report 'public_dashboard_assets=pass'`,
		`Open from the installed Home Screen Sysmon app on your device during the hold:`,
		`confirm the status strip shows app mode`,
		`device_hold_instruction=home_screen_open_status_strip_tap`,
		`Observed a fresh device dashboard client check during the deployed hold, but it was not in Home Screen standalone mode.`,
		`Observed a fresh device Home Screen status-strip tap during the deployed hold.`,
		`Observed a fresh device Home Screen dashboard client check, but it did not include the required status-strip tap.`,
		`Observed a fresh device dashboard client check, but it was not running the current dashboard build.`,
		`Write-Report 'device_client_seen=pass'`,
		`Write-Report 'device_client_seen=not_interactive'`,
		`Write-Report 'device_client_seen=stale_dashboard_build'`,
		`$script:InstalledDeviceHomeScreen = 'pass'`,
		`$script:InstalledDeviceHomeScreen = 'not_verified'`,
		`Write-Report "installed_device_home_screen=$script:InstalledDeviceHomeScreen"`,
		`Write-Report 'device_client_seen=not_standalone'`,
		`Write-Report 'device_client_seen=stale_client'`,
		`Write-Report 'device_client_seen=unexpected_client'`,
		`Write-Report 'device_client_seen=not_observed'`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-deployed-windows.ps1 missing %q", needle)
		}
	}

	settingsStart := strings.Index(script, "function Test-SettingsSameOriginRoundTrip")
	settingsEnd := strings.Index(script, "function Test-ClientCheckSameOriginRoundTrip")
	if settingsStart < 0 || settingsEnd < 0 || settingsEnd <= settingsStart {
		t.Fatalf("verify-deployed-windows.ps1 missing settings roundtrip function boundaries")
	}
	settingsFunction := script[settingsStart:settingsEnd]
	for _, needle := range []string{
		`$statusAfterSettings = Invoke-RestMethod -Method Get -Uri "$Url/api/status" -TimeoutSec 5`,
		`$statusSettings = $statusAfterSettings.settings`,
		`settings status.thresholds.$key roundtrip mismatch`,
	} {
		if !strings.Contains(settingsFunction, needle) {
			t.Fatalf("Test-SettingsSameOriginRoundTrip missing %q", needle)
		}
	}
}

func TestWindowsDeployedVerifierWritesSingleStaleClientOutcome(t *testing.T) {
	data, err := os.ReadFile("verify-deployed-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "function Write-ClientCheckResult")
	end := strings.Index(script, "function Get-ClientCheckEntries")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("verify-deployed-windows.ps1 missing Write-ClientCheckResult boundaries")
	}

	resultFunction := script[start:end]
	const staleClientReport = `Write-Report 'device_client_seen=stale_client'`
	if count := strings.Count(resultFunction, staleClientReport); count != 1 {
		t.Fatalf("Write-ClientCheckResult has %d stale-client reports, want 1", count)
	}
}
