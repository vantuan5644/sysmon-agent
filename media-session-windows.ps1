#requires -Version 5.1
# Runs on the logged-in desktop through the native -control-emit helper.
$ErrorActionPreference = 'Stop'

function Write-MediaResult([bool]$Handled, [bool]$Applied, [string]$ErrorText = '') {
    @{ handled = $Handled; applied = $Applied; error = $ErrorText } | ConvertTo-Json -Compress
}

try {
    Add-Type -AssemblyName System.Runtime.WindowsRuntime
    $managerType = [Windows.Media.Control.GlobalSystemMediaTransportControlsSessionManager,Windows.Media.Control,ContentType=WindowsRuntime]
} catch {
    # Older Windows releases and legacy players use the keyboard path.
    Write-MediaResult $false $false
    exit 0
}

try {
    $asTask = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
        $_.Name -eq 'AsTask' -and $_.IsGenericMethod -and $_.GetParameters().Count -eq 1 -and
        $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1'
    } | Select-Object -First 1
    function Wait-MediaOperation($Operation, $ResultType) {
        $task = $asTask.MakeGenericMethod($ResultType).Invoke($null, @($Operation))
        if (-not $task.Wait(2000)) { throw 'Windows media control timed out' }
        $task.GetAwaiter().GetResult()
    }
    $manager = Wait-MediaOperation ($managerType::RequestAsync()) $managerType
    $session = $manager.GetCurrentSession()
    if ($null -eq $session) {
        Write-MediaResult $false $false
        exit 0
    }
    $playback = $session.GetPlaybackInfo()
    if ($playback.Controls.IsPlayPauseToggleEnabled) {
        $operation = $session.TryTogglePlayPauseAsync()
    } elseif ([string]$playback.PlaybackStatus -eq 'Playing' -and $playback.Controls.IsPauseEnabled) {
        $operation = $session.TryPauseAsync()
    } elseif ($playback.Controls.IsPlayEnabled) {
        $operation = $session.TryPlayAsync()
    } else {
        Write-MediaResult $true $false 'The current media session does not allow play/pause'
        exit 0
    }
    $accepted = Wait-MediaOperation $operation ([bool])
    if ($accepted) { Write-MediaResult $true $true }
    else { Write-MediaResult $true $false 'The current media session rejected play/pause' }
} catch {
    # Do not also send a key after a failed session request: a delayed command
    # could otherwise toggle the player twice.
    Write-MediaResult $true $false $_.Exception.Message
}
