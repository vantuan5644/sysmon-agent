; ============================================================
; Sysmon Agent — Windows installer (NSIS)
; ============================================================
; Produces a double-clickable Setup .exe for non-technical users:
;   - installs sysmon-agent.exe + install-windows.ps1 to Program Files
;   - elevates (RequestExecutionLevel admin) and runs install-windows.ps1,
;     which registers + starts the SysmonAgent service, opens the firewall,
;     and waits for /readyz
;   - adds Start Menu / Desktop shortcuts to the dashboard
;   - registers a clean uninstaller (Add/Remove Programs)
;
; The installer REUSES the existing install-windows.ps1 for all service /
; firewall / recovery / readiness logic, so there is a single source of truth.
;
; Build (cross-compilable — makensis runs natively on Linux/macOS/Windows):
;   makensis -DVERSION=0.1.0 dist/installer.nsi
;   # or use the wrapper: ./dist/build-installer.sh 0.1.0
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

!define MUI_ICON "packaging\sysmon.ico"
!define MUI_UNICON "packaging\sysmon.ico"
!define MUI_ABORTWARNING

; Welcome / license / directory / installing / finish
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\LICENSE"
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
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
  SetOutPath "$INSTDIR"
  File "out\sysmon-agent.exe"
  File "..\install-windows.ps1"

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

  DetailPrint "Registering and starting the SysmonAgent service..."
  nsExec::ExecToLog 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$INSTDIR\install-windows.ps1" -Action Install'
  Pop $0 ; exit code
  ${If} $0 != 0
    DetailPrint "WARNING: install-windows.ps1 exited with code $0."
    DetailPrint "The files were installed, but the service may not be running."
    DetailPrint "Re-run from an admin PowerShell:  .\install-windows.ps1 -Action Install"
    MessageBox MB_ICONEXCLAMATION|MB_OK "The service setup script reported an error (code $0).$\r$\n$\r$\nFiles were installed to $INSTDIR, but the service may not be running.$\r$\nOpen the details window to see the script output, or re-run:$\r$\n  install-windows.ps1 -Action Install"
  ${EndIf}
SectionEnd

Section "Start Menu shortcut" SecStartMenu
  SetOutPath "$INSTDIR"
  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  ; Internet-shortcut (.url) is the most reliable way to open the default
  ; browser at the dashboard from a Start Menu / Desktop icon.
  WriteINIStr "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url" "InternetShortcut" "URL" "${SERVICE_URL}"
  WriteINIStr "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url" "InternetShortcut" "IconFile" "$INSTDIR\sysmon-agent.exe"
  WriteINIStr "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url" "InternetShortcut" "IconIndex" "0"
SectionEnd

Section "Desktop shortcut" SecDesktop
  SetOutPath "$INSTDIR"
  WriteINIStr "$DESKTOP\${APPNAME} Dashboard.url" "InternetShortcut" "URL" "${SERVICE_URL}"
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
Function .onInit
  SectionSetFlags ${SecStartMenu} ${SF_SELECTED}
FunctionEnd

; ============================================================
; Uninstall
; ============================================================
Section "Uninstall"
  ; Stop + delete the service and remove the firewall rule first, while the
  ; binary is still present (install-windows.ps1 resolves it via $PSScriptRoot).
  IfFileExists "$INSTDIR\install-windows.ps1" 0 +3
    DetailPrint "Removing the SysmonAgent service..."
    nsExec::ExecToLog 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$INSTDIR\install-windows.ps1" -Action Uninstall'

  Delete "$INSTDIR\sysmon-agent.exe"
  Delete "$INSTDIR\install-windows.ps1"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${APPNAME}\${APPNAME} Dashboard.url"
  RMDir  "$SMPROGRAMS\${APPNAME}"
  Delete "$DESKTOP\${APPNAME} Dashboard.url"

  DeleteRegKey HKLM "${UNINSTKEY}"
  DeleteRegKey HKLM "${REGKEY}"
SectionEnd
