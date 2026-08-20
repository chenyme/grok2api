@echo off
chcp 65001 >nul
title Grok2API 一键停止

echo =======================================================
echo          Grok2API 一键停止脚本
echo =======================================================
echo.

echo 正在停止占用 8000 端口 (后端) 的进程...
for /f "tokens=5" %%a in ('netstat -aon ^| findstr ":8000" ^| findstr "LISTENING"') do (
    taskkill /F /PID %%a >nul 2>nul
)

echo 正在停止占用 5173 端口 (前端) 的进程...
for /f "tokens=5" %%a in ('netstat -aon ^| findstr ":5173" ^| findstr "LISTENING"') do (
    taskkill /F /PID %%a >nul 2>nul
)

echo 停止完成！
echo =======================================================
timeout /t 3 >nul
