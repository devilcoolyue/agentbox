; Tauri CLI 2.12.1 checks only an empty pv. Microsoft also defines 0.0.0.0
; as missing. Keep separate machine/user evidence; never modify registration.

; Outputs $0=value, $1=Win32 result. Unlike ReadRegStr's single Errors flag,
; access denial/type/size errors stay distinct from ERROR_FILE_NOT_FOUND.
; A Unicode buffer is NSIS_MAX_STRLEN WCHARs, so cbData must be twice that.
!macro AgentboxReadWebView2Value HIVE ROOT KEY FLAGS
  StrCpy $0 ""
  StrCpy $1 "unknown"
  StrCpy $2 0
  IntOp $3 ${NSIS_MAX_STRLEN} * 2
  ClearErrors
  System::Call 'advapi32::RegGetValueW(p ${ROOT}, w "${KEY}", w "pv", i ${FLAGS}, *i .r2, w .r0, *i r3)i.r1'
  ${If} ${Errors}
    StrCpy $1 "unknown"
  ${ElseIf} $1 == 0
    ${If} $2 != 1
      StrCpy $1 1630 ; ERROR_UNSUPPORTED_TYPE
    ${EndIf}
  ${EndIf}
!macroend

!macro AgentboxReadWebView2Versions MACHINE MACHINE_STATUS USER USER_STATUS
  ; RRF_RT_REG_SZ | RRF_ZEROONFAILURE, and explicit 64-bit registry view on x64
  ; to match Tauri's SetContext. No ambient SetRegView changes escape the hook.
  ${If} ${RunningX64}
    !insertmacro AgentboxReadWebView2Value machine 0x80000002 "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\${WEBVIEW2APPGUID}" 0x20010002
  ${Else}
    !insertmacro AgentboxReadWebView2Value machine 0x80000002 "SOFTWARE\Microsoft\EdgeUpdate\Clients\${WEBVIEW2APPGUID}" 0x20000002
  ${EndIf}
  StrCpy ${MACHINE} $0
  StrCpy ${MACHINE_STATUS} $1
  ${If} ${RunningX64}
    !insertmacro AgentboxReadWebView2Value user 0x80000001 "SOFTWARE\Microsoft\EdgeUpdate\Clients\${WEBVIEW2APPGUID}" 0x20010002
  ${Else}
    !insertmacro AgentboxReadWebView2Value user 0x80000001 "SOFTWARE\Microsoft\EdgeUpdate\Clients\${WEBVIEW2APPGUID}" 0x20000002
  ${EndIf}
  StrCpy ${USER} $0
  StrCpy ${USER_STATUS} $1
!macroend

; Positive four-part Windows file version, not VersionCompare's permissive
; numeric-prefix parsing. Unknown/malformed registration cannot mean repaired.
; Only the private repair hook calls this macro once in the Install section.
!macro AgentboxValidWebView2Version VALUE RESULT TAG
  StrCpy ${RESULT} 0
  StrCpy $R0 0 ; character index
  StrCpy $R1 0 ; separators
  StrCpy $R2 0 ; component digits
  StrCpy $R3 0 ; component value
  StrCpy $R4 0 ; any positive digit
  abx_webview_${TAG}_loop:
    StrCpy $R5 "${VALUE}" 1 $R0
    StrCmp $R5 "" abx_webview_${TAG}_end
    StrCmp $R5 "." abx_webview_${TAG}_dot
    StrCmp $R5 "0" abx_webview_${TAG}_digit
    StrCmp $R5 "1" abx_webview_${TAG}_digit
    StrCmp $R5 "2" abx_webview_${TAG}_digit
    StrCmp $R5 "3" abx_webview_${TAG}_digit
    StrCmp $R5 "4" abx_webview_${TAG}_digit
    StrCmp $R5 "5" abx_webview_${TAG}_digit
    StrCmp $R5 "6" abx_webview_${TAG}_digit
    StrCmp $R5 "7" abx_webview_${TAG}_digit
    StrCmp $R5 "8" abx_webview_${TAG}_digit
    StrCmp $R5 "9" abx_webview_${TAG}_digit abx_webview_${TAG}_invalid
  abx_webview_${TAG}_digit:
    IntOp $R2 $R2 + 1
    IntCmp $R2 5 0 0 abx_webview_${TAG}_invalid
    IntOp $R3 $R3 * 10
    IntOp $R3 $R3 + $R5
    IntCmp $R3 65535 0 0 abx_webview_${TAG}_invalid
    StrCmp $R5 "0" abx_webview_${TAG}_next
    StrCpy $R4 1
    Goto abx_webview_${TAG}_next
  abx_webview_${TAG}_dot:
    StrCmp $R2 0 abx_webview_${TAG}_invalid
    IntOp $R1 $R1 + 1
    IntCmp $R1 3 0 0 abx_webview_${TAG}_invalid
    StrCpy $R2 0
    StrCpy $R3 0
  abx_webview_${TAG}_next:
    IntOp $R0 $R0 + 1
    Goto abx_webview_${TAG}_loop
  abx_webview_${TAG}_end:
    StrCmp $R1 3 0 abx_webview_${TAG}_invalid
    StrCmp $R2 0 abx_webview_${TAG}_invalid
    StrCmp $R4 1 0 abx_webview_${TAG}_invalid
    StrCpy ${RESULT} 1
  abx_webview_${TAG}_invalid:
!macroend

!macro AgentboxSelectWebView2Version MACHINE MACHINE_STATUS USER USER_STATUS VALUE DECISION TAG
  StrCpy ${VALUE} ""
  StrCpy ${DECISION} "unknown"
  ${If} ${MACHINE_STATUS} == 0
    !insertmacro AgentboxValidWebView2Version ${MACHINE} $0 ${TAG}_machine
    ${If} $0 == 1
      StrCpy ${VALUE} ${MACHINE}
      StrCpy ${DECISION} "ready"
    ${EndIf}
  ${EndIf}
  ${If} ${DECISION} != "ready"
    ${If} ${USER_STATUS} == 0
      !insertmacro AgentboxValidWebView2Version ${USER} $0 ${TAG}_user
      ${If} $0 == 1
        StrCpy ${VALUE} ${USER}
        StrCpy ${DECISION} "ready"
      ${EndIf}
    ${EndIf}
  ${EndIf}
  ${If} ${DECISION} != "ready"
    ; Repair only when both hives positively establish documented absence.
    ; Missing key/value (2/3) and successful empty/zero reads are acceptable.
    ${If} ${MACHINE_STATUS} == 0
    ${OrIf} ${MACHINE_STATUS} == 2
    ${OrIf} ${MACHINE_STATUS} == 3
      ${If} ${MACHINE} == ""
      ${OrIf} ${MACHINE} == "0.0.0.0"
        ${If} ${USER_STATUS} == 0
        ${OrIf} ${USER_STATUS} == 2
        ${OrIf} ${USER_STATUS} == 3
          ${If} ${USER} == ""
          ${OrIf} ${USER} == "0.0.0.0"
            StrCpy ${DECISION} "missing"
          ${EndIf}
        ${EndIf}
      ${EndIf}
    ${EndIf}
  ${EndIf}
!macroend

!macro AgentboxPrepareWebView2Directory
  InitPluginsDir
!macroend

!macro AgentboxExtractWebView2Installer
  File "/oname=$PLUGINSDIR\AgentboxWebView2Repair.exe" "${WEBVIEW2INSTALLERPATH}"
!macroend

!macro NSIS_HOOK_PREINSTALL
  !if "${INSTALLWEBVIEW2MODE}" == "offlineInstaller"
    ${If} $UpdateMode <> 1
      Push $R6
      StrCpy $R6 0
      ${If} ${Errors}
        StrCpy $R6 1
      ${EndIf}
      Push $0
      Push $1
      Push $2
      Push $3
      Push $4
      Push $7
      Push $8
      Push $R0
      Push $R1
      Push $R2
      Push $R3
      Push $R4
      Push $R5
      Push $R7
      Push $R8
      !insertmacro AgentboxReadWebView2Versions $R7 $7 $R8 $8
      !insertmacro AgentboxSelectWebView2Version $R7 $7 $R8 $8 $4 $1 before
      ${If} $1 == "unknown"
        Abort "WebView2 registration could not be verified. Agentbox was not installed."
      ${EndIf}
      ${If} $1 == "missing"
        DetailPrint "Installing the missing WebView2 Runtime from this offline package..."
        ClearErrors
        !insertmacro AgentboxPrepareWebView2Directory
        ${If} ${Errors}
          Abort "Cannot prepare the WebView2 installer directory. Agentbox was not installed."
        ${EndIf}
        ClearErrors
        !insertmacro AgentboxExtractWebView2Installer
        ${If} ${Errors}
          Delete "$PLUGINSDIR\AgentboxWebView2Repair.exe"
          Abort "Cannot extract the WebView2 Runtime installer. Agentbox was not installed."
        ${EndIf}
        ClearErrors
        ExecWait '"$PLUGINSDIR\AgentboxWebView2Repair.exe" ${WEBVIEW2INSTALLERARGS} /install' $0
        ${If} ${Errors}
          Delete "$PLUGINSDIR\AgentboxWebView2Repair.exe"
          Abort "The WebView2 Runtime installer could not be started. Agentbox was not installed."
        ${EndIf}
        Delete "$PLUGINSDIR\AgentboxWebView2Repair.exe"
        ${If} $0 <> 0
          Abort "The WebView2 Runtime installer did not succeed. Agentbox was not installed."
        ${EndIf}
        !insertmacro AgentboxReadWebView2Versions $R7 $7 $R8 $8
        !insertmacro AgentboxSelectWebView2Version $R7 $7 $R8 $8 $4 $1 after
      ${EndIf}
      ${If} $1 != "ready"
        Abort "WebView2 Runtime registration is missing or invalid. Agentbox was not installed."
      ${EndIf}
      Pop $R8
      Pop $R7
      Pop $R5
      Pop $R4
      Pop $R3
      Pop $R2
      Pop $R1
      Pop $R0
      Pop $8
      Pop $7
      Pop $4
      Pop $3
      Pop $2
      Pop $1
      Pop $0
      ${If} $R6 == 1
        Pop $R6
        SetErrors
      ${Else}
        Pop $R6
        ClearErrors
      ${EndIf}
    ${EndIf}
  !endif
!macroend
