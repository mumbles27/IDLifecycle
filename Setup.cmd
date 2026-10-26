@echo off
rem IDLifecycle setup: double-click to start the graphical installer / uninstaller
rem (scripts\Start-IDLifecycleSetup.ps1). It asks for administrator rights itself.
setlocal
set "IDL_SETUP_GUI=%~dp0scripts\Start-IDLifecycleSetup.ps1"
if not exist "%IDL_SETUP_GUI%" goto :nogui

set "IDL_PWSH="
for /f "delims=" %%P in ('where pwsh.exe 2^>nul') do if not defined IDL_PWSH set "IDL_PWSH=%%P"
if not defined IDL_PWSH if exist "%ProgramFiles%\PowerShell\7\pwsh.exe" set "IDL_PWSH=%ProgramFiles%\PowerShell\7\pwsh.exe"
if not defined IDL_PWSH goto :nopwsh

"%IDL_PWSH%" -NoProfile -ExecutionPolicy Bypass -STA -File "%IDL_SETUP_GUI%" %*
set "IDL_RC=%ERRORLEVEL%"
if not "%IDL_RC%"=="0" (
    echo.
    echo IDLifecycle setup ended with exit code %IDL_RC%.
    pause
)
exit /b %IDL_RC%

:nopwsh
echo IDLifecycle setup needs PowerShell 7 - pwsh.exe was not found.
echo Install it from https://aka.ms/powershell and run Setup.cmd again.
pause
exit /b 3

:nogui
echo "%IDL_SETUP_GUI%" is missing; this does not look like a complete IDLifecycle release folder.
pause
exit /b 2
