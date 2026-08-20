@echo off
chcp 65001 >nul
title Grok2API 一键启动器

echo =======================================================
echo          Grok2API 一键启动脚本 (前后端分离)
echo =======================================================
echo.

:: 环境变量保障
set "PATH=C:\Program Files\Go\bin;D:\EngSofeware\nodejs;%PATH%"
set "GOTMPDIR=%USERPROFILE%\AppData\Local\Temp"

:: 检查 Go 环境
where go >nul 2>nul
if %errorlevel% neq 0 (
    echo [错误] 未检测到 Go 环境，请确认是否已安装 Go。
    pause
    exit /b 1
)

:: 检查 Node.js 环境
where npm >nul 2>nul
if %errorlevel% neq 0 (
    echo [错误] 未检测到 Node.js / npm 环境，请确认是否已安装 Node.js。
    pause
    exit /b 1
)

echo [1/2] 正在启动后端服务 (端口: 8000)...
start "Grok2API - 后端服务 (Port 8000)" cmd /k "cd /d "%~dp0backend" && go run ./cmd/grok2api --config ../config.yaml"

echo [2/2] 正在启动前端服务 (端口: 5173)...
start "Grok2API - 前端服务 (Port 5173)" cmd /k "cd /d "%~dp0frontend" && npm run dev"

echo.
echo =======================================================
echo                  服务已在后台启动！
echo =======================================================
echo   - 前端管理页面: http://localhost:5173 (或 http://[::1]:5173)
echo   - 后端 API 地址: http://localhost:8000 (或 http://[::1]:8000)
echo   - 管理员账号: admin
echo   - 初始密码:   anhuang520
echo.
echo 提示: 请勿关闭弹出的后端与前端命令行窗口。
echo =======================================================
echo.
pause
