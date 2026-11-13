; ============================================================
; Sysmon Agent — Windows installer (NSIS)
; ============================================================
; Produces a double-clickable Setup .exe for non-technical users:
;   - installs sysmon-agent.exe + install-windows.ps1 to Program Files
;   - elevates (RequestExecutionLevel admin) and runs install-windows.ps1,
;     which registers + starts the SysmonAgent service, opens the firewall,
;     and waits for /readyz
;   - asks how the GPU overheat guard should act (a page of its own), keeping
;     what an installed service runs with on an upgrade
;   - adds Start Menu / Desktop shortcuts to the dashboard, and a Start Menu
;     "Pair a device" shortcut that prints a new pairing PIN
;   - offers "Pair a device now" on its last page: a console with a PIN and a
;     QR code that opens the dashboard with that PIN filled in
;   - registers a clean uninstaller (Add/Remove Programs)
;
; The installer REUSES the existing install-windows.ps1 for all service /
; firewall / recovery / readiness logic, so there is a single source of truth.
;
; Build (cross-compilable — makensis runs natively on Linux/macOS/Windows):
;   makensis -DVERSION=0.1.0 dist/installer.nsi
;   # or use the wrapper: ./dist/build-installer.sh 0.1.0
;
; Silent install (/S) takes the guard settings as switches, each optional:
;   SysmonAgent-Setup.exe /S /GPUGUARD=act /GPUHOTSPOTWARN=88 /GPUHOTSPOTCRIT=93 /GPUDELTAWARN=20
; A silent install opens no pairing window; pair from the Start menu later.
;
; Output: dist/out/SysmonAgent-Setup-<VERSION>.exe
; ============================================================

!ifndef VERSION
  !define VERSION "0.1.0"
!endif

; --- product identity ---
!define APPNAME        "Sysmon Agent"
!define APPNAME_SHORT  "Sysmon"
!define PUBLISHER      "Sysmon Agent"
!define REGKEY         "Software\${APPNAME_SHORT}"
!define UNINSTKEY      "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME_SHORT}"
!define SERVICE_URL    "http://localhost:9099/"
!define SERVICE_NAME   "SysmonAgent"

Name "${APPNAME} ${VERSION}"
OutFile "out\${APPNAME_SHORT}Agent-Setup-${VERSION}.exe"
Unicode True
ShowInstDetails show
ShowUnInstDetails show
SetCompressor /SOLID lzma
RequestExecutionLevel admin

InstallDir "$PROGRAMFILES64\${APPNAME}"
InstallDirRegKey HKLM "${REGKEY}" "InstallDir"

; --- modern UI ---
!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "x64.nsh"
!include "nsDialogs.nsh"
!include "FileFunc.nsh"
!include "WordFunc.nsh"

; GPU overheat guard, as install-windows.ps1 -GpuGuard and friends take it.
; The defaults are the agent's own; only a changed value is passed on.
Var GpuGuardMode
Var GpuHotspotWarn
Var GpuHotspotCrit
Var GpuDeltaWarn
Var ServicePort
; The installed service's command line, empty on a fresh install.
Var PreviousImagePath
; 1 when the installed service already ran with -pairing, so its devices
; stay paired across this upgrade.
Var PairedBefore
Var ServiceRunning
Var GpuRadioWarn
Var GpuRadioAct
Var GpuRadioOff
Var GpuWarnField
Var GpuCritField
Var GpuDeltaField

!define MUI_ICON "packaging\sysmon.ico"
!define MUI_UNICON "packaging\sysmon.ico"
!define MUI_ABORTWARNING

; Welcome / license / directory / installing / finish
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\LICENSE"
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
Page custom GpuGuardPage GpuGuardPageLeave
!insertmacro MUI_PAGE_INSTFILES
; The install itself opens no pairing window (-SkipPair): "Pair a device now"
; opens one in a console, with a QR code, after Finish.
!define MUI_FINISHPAGE_TEXT "Sysmon Agent is installed.$\r$\n$\r$\nOther devices see this computer's data only once they are paired. Pair a device now opens a window with a PIN, and a QR code to scan with the device's camera.$\r$\n$\r$\nLater, open Start, Sysmon Agent, Pair a device, or choose Pair another device on a dashboard that is already paired."
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Pair a device now"
!define MUI_FINISHPAGE_RUN_FUNCTION PairDeviceNow
!define MUI_PAGE_CUSTOMFUNCTION_SHOW FinishPageShow
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_WELCOME
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_UNPAGE_FINISH

!insertmacro MUI_LANGUAGE "English"

; ============================================================
; Install sections
; ============================================================
Section "!${APPNAME} (required)" SecCore
  SectionIn RO

  ; --- in-place upgrade: release the exe lock BEFORE overwriting it ---------
  ; On an upgrade the old service is still running and holds a lock on
  ; sysmon-agent.exe, so the File directive below would fail with a sharing
  ; violation and the installer would silently ship the old binary. `sc stop`
  ; is asynchronous, so issuing it is not enough: we must wait for the service
  ; to actually reach STOPPED, which is what releases the lock.
  ; install-windows.ps1 -Action Install stops/reconfigures/starts the service
  ; anyway, but that runs *after* the copy — too late to help here.
  nsExec::Exec 'cmd /c sc query "${SERVICE_NAME}" >nul 2>&1'
  Pop $2 ; 0 when the service already exists (i.e. this is an upgrade)
  ${If} $2 == 0
    DetailPrint "Existing install detected - stopping the ${SERVICE_NAME} service before upgrading..."
    nsExec::ExecToLog 'cmd /c sc stop "${SERVICE_NAME}"'
    Pop $3
    ; Poll for STOPPED, up to ~15 s (30 x 500 ms).
    StrCpy $4 0
    upgrade_wait_loop:
      nsExec::Exec 'cmd /c sc query "${SERVICE_NAME}" | find "STOPPED" >nul'
      Pop $5
      ${If} $5 == 0
        DetailPrint "Service stopped; upgrading files."
        Goto upgrade_wait_done
      ${EndIf}
      IntOp $4 $4 + 1
      ${If} $4 >= 30
        DetailPrint "WARNING: the service did not stop within 15 seconds; the file copy may fail."
        Goto upgrade_wait_done
      ${EndIf}
      Sleep 500
      Goto upgrade_wait_loop
    upgrade_wait_done:
  ${EndIf}

  SetOutPath "$INSTDIR"
  ; ClearErrors/IfErrors around the copy so a still-locked exe surfaces as a
  ; clear message instead of NSIS's generic abort dialog.
  ClearErrors
  File "out\sysmon-agent.exe"
  File "..\install-windows.ps1"
  ${If} ${Errors}
    DetailPrint "ERROR: could not write $INSTDIR\sysmon-agent.exe (the file may still be locked)."
    MessageBox MB_ICONSTOP|MB_OK "Setup could not replace sysmon-agent.exe - it is still in use.$\r$\n$\r$\nStop the ${SERVICE_NAME} service (or reboot) and run Setup again."
    Abort
  ${EndIf}

  ; Remember install dir for upgrades / uninstall.
  WriteRegStr HKLM "${REGKEY}" "InstallDir" "$INSTDIR"

  ; Add/Remove Programs entry.
  WriteRegStr   HKLM "${UNINSTKEY}" "DisplayName"     "${APPNAME}"
  WriteRegStr   HKLM "${UNINSTKEY}" "DisplayVersion"  "${VERSION}"
  WriteRegStr   HKLM "${UNINSTKEY}" "Publisher"       "${PUBLISHER}"
  WriteRegStr   HKLM "${UNINSTKEY}" "DisplayIcon"     "$INSTDIR\sysmon-agent.exe"
  WriteRegStr   HKLM "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   HKLM "${UNINSTKEY}" "URLInfoAbout"    "${SERVICE_URL}"
  WriteRegStr   HKLM "${UNINSTKEY}" "UninstallString" "$\"$INSTDIR\uninstall.exe$\""
  WriteRegStr   HKLM "${UNINSTKEY}" "QuietUninstallString" "$\"$INSTDIR\uninstall.exe$\" /S"
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1
  WriteUninstaller "$INSTDIR\uninstall.exe"

  ; The page's values, every time: -KeepServiceArguments keeps whatever is not
  ; named, so a value put back to its default must be named too. 0 leaves a
  ; line to the agent's default, which then follows a later change of it.
  StrCpy $6 " -GpuGuard $GpuGuardMode"
  ${If} $GpuHotspotWarn != 90
    StrCpy $6 "$6 -GpuHotspotWarn $GpuHotspotWarn"
  ${Else}
    StrCpy $6 "$6 -GpuHotspotWarn 0"
  ${EndIf}
  ${If} $GpuHotspotCrit != 95
    StrCpy $6 "$6 -GpuHotspotCrit $GpuHotspotCrit"
  ${Else}
    StrCpy $6 "$6 -GpuHotspotCrit 0"
  ${EndIf}
  ${If} $GpuDeltaWarn != 20
    StrCpy $6 "$6 -GpuDeltaWarn $GpuDeltaWarn"
  ${Else}
    StrCpy $6 "$6 -GpuDeltaWarn 0"
  ${EndIf}

  DetailPrint "Registering and starting the ${SERVICE_NAME} service..."
  ; The 64-bit Windows PowerShell. The 32-bit one this installer would start
  ; reads $env:ProgramFiles as Program Files (x86), where the script's own
  ; Tailscale lookup then misses C:\Program Files\Tailscale.
  ${DisableX64FSRedirection}
  nsExec::ExecToLog '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -ExecutionPolicy Bypass -File "$INSTDIR\install-windows.ps1" -Action Install -SkipPair -KeepServiceArguments$6'
  Pop $0 ; install-windows.ps1 exit code
  ${EnableX64FSRedirection}

  ; The running service is the source of truth. install-windows.ps1 now
  ; downgrades a slow first-boot readiness check and firewall hiccups to
  ; warnings, so a non-zero exit code no longer means the install failed.
  ; Confirm the service actually reports RUNNING before alarming the user.
  nsExec::Exec 'cmd /c sc query "${SERVICE_NAME}" | find "RUNNING"'
  Pop $1 ; 0 when the service is RUNNING

  StrCpy $ServiceRunning 0
  ${If} $1 == 0
    StrCpy $ServiceRunning 1
    DetailPrint "Service ${SERVICE_NAME} is running."
    ${If} $0 != 0
      DetailPrint "(Setup script reported warnings, code $0 - see output above; the service is running.)"
    ${EndIf}
  ${Else}
    DetailPrint "WARNING: the ${SERVICE_NAME} service is not running (script exit code $0)."
    DetailPrint "The files were installed. Re-run from an admin PowerShell:"
    DetailPrint "  .\install-windows.ps1 -Action Install"
    MessageBox MB_ICONEXCLAMATION|MB_OK "Setup installed the files to $INSTDIR, but the ${SERVICE_NAME} service is not running yet.$\r$\n$\r$\nOpen the details window to see the script output, or re-run from an elevated PowerShell:$\r$\n  install-windows.ps1 -Action Install"
  ${EndIf}
SectionEnd

Section "Start Menu shortcut" SecStartMenu
  SetOutPath "$INSTDIR"
  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  ; Internet-shortcut (.url) is the most reliable way to open the default
  ; browser at the dashboard from a Start Menu / Desktop icon.
  WriteINIStr "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url" "InternetShortcut" "URL" "http://localhost:$ServicePort/"
  WriteINIStr "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url" "InternetShortcut" "IconFile" "$INSTDIR\sysmon-agent.exe"
  WriteINIStr "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url" "InternetShortcut" "IconIndex" "0"
  ; Prints a new 10-minute pairing PIN. The script asks for elevation itself
  ; and waits for Enter, so the PIN stays on screen.
  CreateShortCut "$SMPROGRAMS\${APPNAME}\Pair a device.lnk" "$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" '-NoProfile -ExecutionPolicy Bypass -File "$INSTDIR\install-windows.ps1" -Action Pair -PauseAtEnd' "$INSTDIR\sysmon-agent.exe" 0
SectionEnd

Section "Desktop shortcut" SecDesktop
  SetOutPath "$INSTDIR"
  WriteINIStr "$DESKTOP\${APPNAME} Dashboard.url" "InternetShortcut" "URL" "http://localhost:$ServicePort/"
  WriteINIStr "$DESKTOP\${APPNAME} Dashboard.url" "InternetShortcut" "IconFile" "$INSTDIR\sysmon-agent.exe"
  WriteINIStr "$DESKTOP\${APPNAME} Dashboard.url" "InternetShortcut" "IconIndex" "0"
SectionEnd

; ============================================================
; Component descriptions
; ============================================================
LangString DESC_SecCore      ${LANG_ENGLISH} "Install Sysmon Agent and start it as a Windows service (required)."
LangString DESC_SecStartMenu ${LANG_ENGLISH} "Add a Start Menu shortcut that opens the dashboard in your browser."
LangString DESC_SecDesktop   ${LANG_ENGLISH} "Add a desktop shortcut that opens the dashboard in your browser."

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecCore}      $(DESC_SecCore)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecStartMenu} $(DESC_SecStartMenu)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop}   $(DESC_SecDesktop)
!insertmacro MUI_FUNCTION_DESCRIPTION_END

; ============================================================
; .onSelChange — keep Start Menu on by default, Desktop off (opt-in)
; ============================================================
; ReadServiceFlag copies the value after FLAG in the installed service's
; command line, up to END, into VAR. VAR keeps its value when FLAG is absent.
; END is appended first, for a value that ends the line.
!macro ReadServiceFlag FLAG END VAR
  ClearErrors
  ${WordFind} "$PreviousImagePath" `${FLAG}` "E+1}" $R0
  ${IfNot} ${Errors}
    ${WordFind} `$R0${END}` `${END}` "+1{" $R1
    StrCpy ${VAR} $R1
  ${EndIf}
!macroend

!macro ReadSwitch PARAMETERS SWITCH VAR
  ClearErrors
  ${GetOptions} "${PARAMETERS}" "${SWITCH}" $R1
  ${IfNot} ${Errors}
    StrCpy ${VAR} $R1
  ${EndIf}
!macroend

Function .onInit
  SectionSetFlags ${SecStartMenu} ${SF_SELECTED}

  StrCpy $GpuGuardMode "warn"
  StrCpy $GpuHotspotWarn 90
  StrCpy $GpuHotspotCrit 95
  StrCpy $GpuDeltaWarn 20
  StrCpy $ServicePort 9099
  StrCpy $PairedBefore 0
  StrCpy $ServiceRunning 0

  ; An upgrade starts from what the installed service runs with, since
  ; install-windows.ps1 rebuilds its command line from the switches it gets.
  ReadRegStr $PreviousImagePath HKLM "SYSTEM\CurrentControlSet\Services\${SERVICE_NAME}" "ImagePath"
  ${If} $PreviousImagePath != ""
    !insertmacro ReadServiceFlag "-gpu-guard " " " $GpuGuardMode
    !insertmacro ReadServiceFlag "-gpu-hotspot-warn " " " $GpuHotspotWarn
    !insertmacro ReadServiceFlag "-gpu-hotspot-crit " " " $GpuHotspotCrit
    !insertmacro ReadServiceFlag "-gpu-delta-warn " " " $GpuDeltaWarn
    !insertmacro ReadServiceFlag "-port " " " $ServicePort
    ClearErrors
    ${WordFind} "$PreviousImagePath" " -pairing " "E+1}" $R0
    ${IfNot} ${Errors}
      StrCpy $PairedBefore 1
    ${EndIf}
  ${EndIf}

  ${GetParameters} $R0
  !insertmacro ReadSwitch $R0 "/GPUGUARD=" $GpuGuardMode
  !insertmacro ReadSwitch $R0 "/GPUHOTSPOTWARN=" $GpuHotspotWarn
  !insertmacro ReadSwitch $R0 "/GPUHOTSPOTCRIT=" $GpuHotspotCrit
  !insertmacro ReadSwitch $R0 "/GPUDELTAWARN=" $GpuDeltaWarn

  ; The page checks what a person types; a silent install stops here instead
  ; of handing install-windows.ps1 a value it refuses after the service has
  ; already been stopped.
  ${If} ${Silent}
    Call CheckGpuGuard
    Pop $R1
    ${If} $R1 != ""
      SetErrorLevel 2
      Quit
    ${EndIf}
  ${EndIf}
FunctionEnd

; GpuLineInRange leaves an error message in $R2 when VALUE is outside the
; range install-windows.ps1 accepts for that switch.
!macro GpuLineInRange VALUE LOW HIGH WHAT
  ${If} ${VALUE} < ${LOW}
  ${OrIf} ${VALUE} > ${HIGH}
    StrCpy $R2 "${WHAT} must be between ${LOW} and ${HIGH} C."
  ${EndIf}
!macroend

; CheckGpuGuard pushes an error message, or "" when the settings are usable.
Function CheckGpuGuard
  StrCpy $R2 ""
  ${If} $GpuGuardMode != "off"
  ${AndIf} $GpuGuardMode != "warn"
  ${AndIf} $GpuGuardMode != "act"
    StrCpy $R2 "The GPU guard mode must be off, warn or act."
  ${EndIf}
  !insertmacro GpuLineInRange $GpuHotspotWarn 70 105 "The hotspot warning"
  !insertmacro GpuLineInRange $GpuHotspotCrit 80 110 "The critical hotspot"
  !insertmacro GpuLineInRange $GpuDeltaWarn 10 40 "The hotspot-to-core warning"
  ${If} $R2 == ""
  ${AndIf} $GpuHotspotWarn >= $GpuHotspotCrit
    StrCpy $R2 "The hotspot warning must be below the critical hotspot."
  ${EndIf}
  Push $R2
FunctionEnd

; ============================================================
; GPU overheat guard page
; ============================================================
Function GpuGuardPage
  !insertmacro MUI_HEADER_TEXT "GPU overheat guard" "What Sysmon does when an NVIDIA graphics card runs hot."
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}

  ${NSD_CreateLabel} 0 0 100% 20u "Sysmon reads the hottest spot on an NVIDIA graphics card every 2 seconds. On a computer without one, this page changes nothing."
  Pop $0

  ${NSD_CreateRadioButton} 0 24u 100% 12u "&Warn: alarms on this computer and on the dashboards (recommended)"
  Pop $GpuRadioWarn
  ${NSD_AddStyle} $GpuRadioWarn ${WS_GROUP}
  ${NSD_CreateRadioButton} 0 37u 100% 12u "&Act: the alarms, then at the critical line lower the power limit and end GPU apps"
  Pop $GpuRadioAct
  ${NSD_CreateRadioButton} 0 50u 100% 12u "&Off"
  Pop $GpuRadioOff
  ${If} $GpuGuardMode == "act"
    ${NSD_Check} $GpuRadioAct
  ${ElseIf} $GpuGuardMode == "off"
    ${NSD_Check} $GpuRadioOff
  ${Else}
    ${NSD_Check} $GpuRadioWarn
  ${EndIf}

  ${NSD_CreateLabel} 0 70u 75% 10u "Warn when the hotspot reaches (70 to 105 C):"
  Pop $0
  ${NSD_AddStyle} $0 ${WS_GROUP}
  ${NSD_CreateNumber} 78% 68u 30u 12u "$GpuHotspotWarn"
  Pop $GpuWarnField
  ${NSD_SetTextLimit} $GpuWarnField 3
  ${NSD_CreateLabel} 0 86u 75% 10u "Critical at (80 to 110 C):"
  Pop $0
  ${NSD_CreateNumber} 78% 84u 30u 12u "$GpuHotspotCrit"
  Pop $GpuCritField
  ${NSD_SetTextLimit} $GpuCritField 3
  ${NSD_CreateLabel} 0 102u 75% 10u "Warn when the hotspot runs this far above the core (10 to 40 C):"
  Pop $0
  ${NSD_CreateNumber} 78% 100u 30u 12u "$GpuDeltaWarn"
  Pop $GpuDeltaField
  ${NSD_SetTextLimit} $GpuDeltaField 2

  ${NSD_CreateLabel} 0 120u 100% 20u "Act spares the desktop, terminals, Sysmon and streaming hosts; apps it ends lose unsaved work. Run Setup again to change this."
  Pop $0

  nsDialogs::Show
FunctionEnd

Function GpuGuardPageLeave
  StrCpy $GpuGuardMode "warn"
  ${NSD_GetState} $GpuRadioAct $0
  ${NSD_GetState} $GpuRadioOff $1
  ${If} $0 == ${BST_CHECKED}
    StrCpy $GpuGuardMode "act"
  ${ElseIf} $1 == ${BST_CHECKED}
    StrCpy $GpuGuardMode "off"
  ${EndIf}
  ${NSD_GetText} $GpuWarnField $GpuHotspotWarn
  ${NSD_GetText} $GpuCritField $GpuHotspotCrit
  ${NSD_GetText} $GpuDeltaField $GpuDeltaWarn
  Call CheckGpuGuard
  Pop $0
  ${If} $0 != ""
    MessageBox MB_ICONEXCLAMATION|MB_OK "$0"
    Abort ; stays on the page
  ${EndIf}
FunctionEnd

; ============================================================
; Finish page
; ============================================================
Function FinishPageShow
  ; An upgrade of an install that already pairs keeps its devices, and a
  ; service that is not running cannot open a pairing window.
  ${If} $PairedBefore == 1
  ${OrIf} $ServiceRunning != 1
    ${NSD_Uncheck} $mui.FinishPage.Run
  ${EndIf}
FunctionEnd

; Runs after Finish, from this elevated installer, so the console asks for no
; second elevation. -PauseAtEnd keeps it open on the PIN.
Function PairDeviceNow
  ${DisableX64FSRedirection}
  Exec '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -ExecutionPolicy Bypass -File "$INSTDIR\install-windows.ps1" -Action Pair -PauseAtEnd'
  ${EnableX64FSRedirection}
FunctionEnd

; ============================================================
; Uninstall
; ============================================================
Section "Uninstall"
  ; Stop + delete the service and remove the firewall rule first, while the
  ; binary is still present (install-windows.ps1 resolves it via $PSScriptRoot).
  IfFileExists "$INSTDIR\install-windows.ps1" 0 +3
    DetailPrint "Removing the SysmonAgent service..."
    ${DisableX64FSRedirection}
    nsExec::ExecToLog '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -ExecutionPolicy Bypass -File "$INSTDIR\install-windows.ps1" -Action Uninstall'
    ${EnableX64FSRedirection}

  Delete "$INSTDIR\sysmon-agent.exe"
  Delete "$INSTDIR\install-windows.ps1"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url"
  Delete "$SMPROGRAMS\${APPNAME}\Pair a device.lnk"
  RMDir  "$SMPROGRAMS\${APPNAME}"
  Delete "$DESKTOP\${APPNAME} Dashboard.url"

  DeleteRegKey HKLM "${UNINSTKEY}"
  DeleteRegKey HKLM "${REGKEY}"
SectionEnd
