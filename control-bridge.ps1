#requires -Version 5.1
# control-bridge.ps1 -- host-control bridge for sysmon-agent (Windows).
#
# Applies one toolbar control and prints a single JSON object describing the
# outcome. On any failure it still prints a JSON object with applied=false and an
# error string, then exits 0, so the Go agent degrades gracefully instead of
# treating a missing control as a failed HTTP request.
#
# Controls:
#   mic_mute     toggle mute on every active capture endpoint (Core Audio)
#   volume_mute  toggle mute on the default playback endpoint (Core Audio)
#   media_toggle send a play/pause media key to the active session
#   lock_screen  lock the interactive workstation
#
# Core Audio endpoint mute is a global device property, so mic_mute / volume_mute
# work even when this runs as the LocalSystem service in session 0. media_toggle
# and lock_screen need the interactive desktop: when run non-interactively they
# are injected into the active console session via CreateProcessAsUser, which
# relaunches the agent's own native binary (-ExePath) with -control-emit. A
# native PE runs reliably across the session boundary; powershell.exe does not
# (it is created but dies in early init before executing any code), so the prior
# -EncodedCommand media path silently did nothing. The agent exe does its one
# Win32 input call and exits, needing no read access to this script.
#
# NOTE: ASCII only -- see the PowerShell scripting notes in the repo root
# CLAUDE.md. Embedded via //go:embed in collector/control_windows.go, so edits
# require a Go rebuild to take effect.

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('mic_mute', 'volume_mute', 'media_toggle', 'lock_screen')]
    [string]$Action,

    # Full path to the agent executable, supplied by the Go layer. Used as the
    # native process injected into the active console session for media_toggle /
    # lock_screen when this bridge runs non-interactively (session 0).
    [string]$ExePath = ''
)

$ErrorActionPreference = 'Stop'

function Write-ControlResult {
    param(
        [bool]$Available,
        [bool]$Applied,
        [string]$State = '',
        [string]$Message = '',
        [string]$ErrorText = ''
    )
    $payload = [ordered]@{
        action    = $Action
        available = $Available
        applied   = $Applied
        state     = $State
        message   = $Message
        error     = $ErrorText
    }
    $payload | ConvertTo-Json -Compress
}

$script:ControlSource = @'
using System;
using System.Runtime.InteropServices;

namespace Sysmon {
    public static class HostControl {

        // ---- Core Audio (mmdeviceapi / endpointvolume) ----
        enum EDataFlow { eRender = 0, eCapture = 1, eAll = 2 }
        enum ERole { eConsole = 0, eMultimedia = 1, eCommunications = 2 }
        const uint DEVICE_STATE_ACTIVE = 0x1;
        const uint CLSCTX_ALL = 23;
        static Guid IID_IAudioEndpointVolume = new Guid("5CDF2C82-841E-4546-9722-0CF74078229A");
        static Guid GUID_EVENT = Guid.Empty;

        [ComImport, Guid("BCDE0395-E52F-467C-8E3D-C4579291692E")]
        class MMDeviceEnumerator { }

        [Guid("A95664D2-9614-4F35-A746-DE8DB63617E6"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
        interface IMMDeviceEnumerator {
            [PreserveSig] int EnumAudioEndpoints(EDataFlow dataFlow, uint dwStateMask, out IMMDeviceCollection ppDevices);
            [PreserveSig] int GetDefaultAudioEndpoint(EDataFlow dataFlow, ERole role, out IMMDevice ppEndpoint);
        }

        [Guid("0BD7A1BE-7A1A-44DB-8397-CC5392387B5E"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
        interface IMMDeviceCollection {
            [PreserveSig] int GetCount(out uint pcDevices);
            [PreserveSig] int Item(uint nDevice, out IMMDevice ppDevice);
        }

        [Guid("D666063F-1587-4E43-81F1-B948E807363F"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
        interface IMMDevice {
            [PreserveSig] int Activate(ref Guid iid, uint dwClsCtx, IntPtr pActivationParams, [MarshalAs(UnmanagedType.IUnknown)] out object ppInterface);
        }

        [Guid("5CDF2C82-841E-4546-9722-0CF74078229A"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
        interface IAudioEndpointVolume {
            [PreserveSig] int RegisterControlChangeNotify(IntPtr pNotify);
            [PreserveSig] int UnregisterControlChangeNotify(IntPtr pNotify);
            [PreserveSig] int GetChannelCount(out uint pnChannelCount);
            [PreserveSig] int SetMasterVolumeLevel(float fLevelDB, ref Guid pguidEventContext);
            [PreserveSig] int SetMasterVolumeLevelScalar(float fLevel, ref Guid pguidEventContext);
            [PreserveSig] int GetMasterVolumeLevel(out float pfLevelDB);
            [PreserveSig] int GetMasterVolumeLevelScalar(out float pfLevel);
            [PreserveSig] int SetChannelVolumeLevel(uint nChannel, float fLevelDB, ref Guid pguidEventContext);
            [PreserveSig] int SetChannelVolumeLevelScalar(uint nChannel, float fLevel, ref Guid pguidEventContext);
            [PreserveSig] int GetChannelVolumeLevel(uint nChannel, out float pfLevelDB);
            [PreserveSig] int GetChannelVolumeLevelScalar(uint nChannel, out float pfLevel);
            [PreserveSig] int SetMute([MarshalAs(UnmanagedType.Bool)] bool bMute, ref Guid pguidEventContext);
            [PreserveSig] int GetMute([MarshalAs(UnmanagedType.Bool)] out bool pbMute);
        }

        static IAudioEndpointVolume GetVolume(IMMDevice device) {
            object o;
            Marshal.ThrowExceptionForHR(device.Activate(ref IID_IAudioEndpointVolume, CLSCTX_ALL, IntPtr.Zero, out o));
            return (IAudioEndpointVolume)o;
        }

        public static string ToggleRenderMute() {
            var en = (IMMDeviceEnumerator)(new MMDeviceEnumerator());
            IMMDevice dev;
            Marshal.ThrowExceptionForHR(en.GetDefaultAudioEndpoint(EDataFlow.eRender, ERole.eMultimedia, out dev));
            var vol = GetVolume(dev);
            bool muted;
            Marshal.ThrowExceptionForHR(vol.GetMute(out muted));
            bool target = !muted;
            Marshal.ThrowExceptionForHR(vol.SetMute(target, ref GUID_EVENT));
            return target ? "muted" : "unmuted";
        }

        public static string ToggleCaptureMute() {
            var en = (IMMDeviceEnumerator)(new MMDeviceEnumerator());
            IMMDeviceCollection col;
            Marshal.ThrowExceptionForHR(en.EnumAudioEndpoints(EDataFlow.eCapture, DEVICE_STATE_ACTIVE, out col));
            uint count;
            Marshal.ThrowExceptionForHR(col.GetCount(out count));
            if (count == 0) {
                throw new Exception("no active capture (microphone) devices found");
            }
            // Derive the target state from the default capture endpoint when present,
            // else from the first device, then apply it to every device so they end
            // up consistent.
            bool current = false;
            IMMDevice def;
            if (en.GetDefaultAudioEndpoint(EDataFlow.eCapture, ERole.eCommunications, out def) == 0 && def != null) {
                Marshal.ThrowExceptionForHR(GetVolume(def).GetMute(out current));
            } else {
                IMMDevice first;
                Marshal.ThrowExceptionForHR(col.Item(0, out first));
                Marshal.ThrowExceptionForHR(GetVolume(first).GetMute(out current));
            }
            bool target = !current;
            for (uint i = 0; i < count; i++) {
                IMMDevice d;
                if (col.Item(i, out d) != 0 || d == null) { continue; }
                try {
                    GetVolume(d).SetMute(target, ref GUID_EVENT);
                } catch { }
            }
            return target ? "muted" : "unmuted";
        }

        // ---- media key + lock (run in the calling session) ----
        [DllImport("user32.dll")]
        static extern void keybd_event(byte bVk, byte bScan, uint dwFlags, UIntPtr dwExtraInfo);
        [DllImport("user32.dll", SetLastError = true)]
        static extern bool LockWorkStation();
        const byte VK_MEDIA_PLAY_PAUSE = 0xB3;
        const uint KEYEVENTF_EXTENDEDKEY = 0x1;
        const uint KEYEVENTF_KEYUP = 0x2;

        public static void MediaPlayPause() {
            keybd_event(VK_MEDIA_PLAY_PAUSE, 0, KEYEVENTF_EXTENDEDKEY, UIntPtr.Zero);
            keybd_event(VK_MEDIA_PLAY_PAUSE, 0, KEYEVENTF_EXTENDEDKEY | KEYEVENTF_KEYUP, UIntPtr.Zero);
        }

        public static void LockScreen() {
            if (!LockWorkStation()) {
                throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error());
            }
        }

        // ---- launch a command in the active console session (session-0 service) ----
        // CharSet.Unicode is REQUIRED: CreateProcessAsUser is bound as the W
        // (Unicode) entry point, so its STARTUPINFO string members (notably
        // lpDesktop = "winsta0\\default") must marshal as wide strings. With the
        // default ANSI marshaling the W function reads lpDesktop as garbage UTF-16,
        // the injected child attaches to a bogus desktop, and user32.dll fails to
        // initialize (ERROR_DLL_INIT_FAILED) the moment the child touches it -- so
        // keybd_event / LockWorkStation never run and media_toggle / lock_screen
        // silently do nothing even though CreateProcessAsUser reported success.
        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
        struct STARTUPINFO {
            public int cb;
            public string lpReserved;
            public string lpDesktop;
            public string lpTitle;
            public int dwX, dwY, dwXSize, dwYSize, dwXCountChars, dwYCountChars, dwFillAttribute, dwFlags;
            public short wShowWindow, cbReserved2;
            public IntPtr lpReserved2, hStdInput, hStdOutput, hStdError;
        }

        [StructLayout(LayoutKind.Sequential)]
        struct PROCESS_INFORMATION {
            public IntPtr hProcess, hThread;
            public int dwProcessId, dwThreadId;
        }

        [DllImport("kernel32.dll")]
        static extern uint WTSGetActiveConsoleSessionId();
        [DllImport("wtsapi32.dll", SetLastError = true)]
        static extern bool WTSQueryUserToken(uint SessionId, out IntPtr phToken);
        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool DuplicateTokenEx(IntPtr hExistingToken, uint dwDesiredAccess, IntPtr lpTokenAttributes, int ImpersonationLevel, int TokenType, out IntPtr phNewToken);
        [DllImport("userenv.dll", SetLastError = true)]
        static extern bool CreateEnvironmentBlock(out IntPtr lpEnvironment, IntPtr hToken, bool bInherit);
        [DllImport("userenv.dll", SetLastError = true)]
        static extern bool DestroyEnvironmentBlock(IntPtr lpEnvironment);
        [DllImport("advapi32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        static extern bool CreateProcessAsUser(IntPtr hToken, string lpApplicationName, string lpCommandLine, IntPtr lpProcessAttributes, IntPtr lpThreadAttributes, bool bInheritHandles, uint dwCreationFlags, IntPtr lpEnvironment, string lpCurrentDirectory, ref STARTUPINFO lpStartupInfo, out PROCESS_INFORMATION lpProcessInformation);
        [DllImport("kernel32.dll", SetLastError = true)]
        static extern bool CloseHandle(IntPtr hObject);
        [DllImport("kernel32.dll")]
        static extern uint WaitForSingleObject(IntPtr hHandle, uint dwMilliseconds);
        [DllImport("kernel32.dll", SetLastError = true)]
        static extern bool GetExitCodeProcess(IntPtr hProcess, out uint lpExitCode);

        const uint MAXIMUM_ALLOWED = 0x02000000;
        const uint CREATE_UNICODE_ENVIRONMENT = 0x00000400;
        const uint CREATE_NO_WINDOW = 0x08000000;
        const int SecurityImpersonation = 2;
        const int TokenPrimary = 1;

        public static void RunInActiveSession(string commandLine) {
            uint sessionId = WTSGetActiveConsoleSessionId();
            if (sessionId == 0xFFFFFFFF) {
                throw new Exception("no active console session (no user is logged in)");
            }
            IntPtr userToken;
            if (!WTSQueryUserToken(sessionId, out userToken)) {
                throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error(), "WTSQueryUserToken failed");
            }
            IntPtr primaryToken = IntPtr.Zero;
            IntPtr env = IntPtr.Zero;
            try {
                if (!DuplicateTokenEx(userToken, MAXIMUM_ALLOWED, IntPtr.Zero, SecurityImpersonation, TokenPrimary, out primaryToken)) {
                    throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error(), "DuplicateTokenEx failed");
                }
                CreateEnvironmentBlock(out env, primaryToken, false);
                var si = new STARTUPINFO();
                si.cb = Marshal.SizeOf(typeof(STARTUPINFO));
                si.lpDesktop = "winsta0\\default";
                PROCESS_INFORMATION pi;
                uint flags = CREATE_UNICODE_ENVIRONMENT | CREATE_NO_WINDOW;
                if (!CreateProcessAsUser(primaryToken, null, commandLine, IntPtr.Zero, IntPtr.Zero, false, flags, env, null, ref si, out pi)) {
                    throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error(), "CreateProcessAsUser failed");
                }
                // CreateProcessAsUser reports success as soon as the process object
                // exists, before the child finishes initializing -- so a launch that
                // "succeeds" can still crash on startup (the way a bad lpDesktop made
                // user32 fail to load). The injected child does one Win32 input call
                // and exits immediately, so wait briefly and treat a confirmed
                // non-zero exit as a failure instead of reporting a false "applied".
                uint waitRc = WaitForSingleObject(pi.hProcess, 4000);
                uint exitCode = 0;
                bool gotExit = GetExitCodeProcess(pi.hProcess, out exitCode);
                CloseHandle(pi.hThread);
                CloseHandle(pi.hProcess);
                if (waitRc == 0 && gotExit && exitCode != 0) {
                    throw new Exception("injected session process exited with code 0x" + exitCode.ToString("X8"));
                }
            } finally {
                if (env != IntPtr.Zero) { DestroyEnvironmentBlock(env); }
                if (primaryToken != IntPtr.Zero) { CloseHandle(primaryToken); }
                CloseHandle(userToken);
            }
        }
    }
}
'@

function Initialize-ControlType {
    if (-not ('Sysmon.HostControl' -as [type])) {
        Add-Type -TypeDefinition $script:ControlSource -Language CSharp
    }
}

function Assert-ExePath {
    if ([string]::IsNullOrWhiteSpace($ExePath)) {
        throw 'agent executable path (-ExePath) was not supplied; cannot inject into the active session'
    }
    if (-not (Test-Path -LiteralPath $ExePath)) {
        throw ('agent executable not found at -ExePath: ' + $ExePath)
    }
}

function Invoke-ControlEmitInActiveSession {
    # Relaunch the agent's own native binary in the active console session with
    # -control-emit <action>; it runs the single Win32 input call (media key or
    # LockWorkStation) on the logged-in user's interactive desktop, then exits.
    # A native PE runs across the session-0 -> session-N boundary where
    # powershell.exe does not.
    param([string]$EmitAction)
    Assert-ExePath
    $commandLine = '"' + $ExePath + '" -control-emit ' + $EmitAction
    [Sysmon.HostControl]::RunInActiveSession($commandLine)
}

function Invoke-MediaToggleInActiveSession {
    Invoke-ControlEmitInActiveSession -EmitAction 'media_play_pause'
}

function Invoke-LockScreenInActiveSession {
    Invoke-ControlEmitInActiveSession -EmitAction 'lock_screen'
}

try {
    Initialize-ControlType
    switch ($Action) {
        'mic_mute' {
            $state = [Sysmon.HostControl]::ToggleCaptureMute()
            Write-ControlResult -Available $true -Applied $true -State $state
        }
        'volume_mute' {
            $state = [Sysmon.HostControl]::ToggleRenderMute()
            Write-ControlResult -Available $true -Applied $true -State $state
        }
        'media_toggle' {
            if ([Environment]::UserInteractive) {
                [Sysmon.HostControl]::MediaPlayPause()
            } else {
                Invoke-MediaToggleInActiveSession
            }
            Write-ControlResult -Available $true -Applied $true -State 'toggled'
        }
        'lock_screen' {
            if ([Environment]::UserInteractive) {
                [Sysmon.HostControl]::LockScreen()
            } else {
                Invoke-LockScreenInActiveSession
            }
            Write-ControlResult -Available $true -Applied $true -State 'locked'
        }
    }
} catch {
    Write-ControlResult -Available $true -Applied $false -ErrorText ($_.Exception.Message)
}

exit 0
