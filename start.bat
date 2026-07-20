@echo off
chcp 65001 >nul 2>&1
title chenyme Grok2API

cd /d "%~dp0"

if not exist "config.yaml" (
    echo [ERROR] 未找到 config.yaml
    echo         请先复制: copy config.example.yaml config.yaml
    pause
    exit /b 1
)

where go >nul 2>&1
if errorlevel 1 (
    echo [ERROR] 未找到 go，请先安装 Go 并加入 PATH
    pause
    exit /b 1
)

netstat -ano | findstr ":8000" | findstr "LISTENING" >nul 2>&1
if not errorlevel 1 (
    echo [OK] Grok2API 已在运行  http://127.0.0.1:8000
    start "" "http://127.0.0.1:8000"
    pause
    exit /b 0
)

echo ================================================
echo   chenyme Grok2API
echo   管理端 / API: http://127.0.0.1:8000
echo   按 Ctrl+C 停止服务
echo ================================================
echo.

start "" "http://127.0.0.1:8000"
cd /d "%~dp0backend"
go run ./cmd/grok2api --config "%~dp0config.yaml"

echo.
pause
