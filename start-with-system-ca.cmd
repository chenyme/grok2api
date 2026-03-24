@echo off
setlocal
cd /d "%~dp0"
powershell -ExecutionPolicy Bypass -NoProfile -File "%~dp0start-grok2api.ps1" %*
set "EXIT_CODE=%ERRORLEVEL%"
endlocal & exit /b %EXIT_CODE%
