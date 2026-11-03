@echo off
setlocal DisableDelayedExpansion
pushd "%~dp0"
if errorlevel 1 exit /b 1
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File "%~dp0launcher.ps1" -Action Stop
set "launcher_exit=%errorlevel%"
pause
popd
endlocal & exit /b %launcher_exit%
