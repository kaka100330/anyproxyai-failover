@echo off
setlocal enabledelayedexpansion

echo ========================================
echo   AnyProxyAi - Build Script (Windows)
echo ========================================
echo.

REM 检查依赖
where go >nul 2>nul || (echo [ERROR] Go not found & pause & exit /b 1)
where node >nul 2>nul || (echo [ERROR] Node.js not found & pause & exit /b 1)

REM 清空 build 目录
echo [0/5] Cleaning build directory...
if exist "build\bin\anyproxyai-windows*" del /q "build\bin\anyproxyai-windows*"
if not exist "build\bin" mkdir "build\bin"

echo [1/5] Installing frontend dependencies...
cd frontend
call npm install
cd ..

echo [2/5] Building frontend...
cd frontend
call npm run build
cd ..

echo [3/5] Downloading Go dependencies...
go mod tidy
go mod download

echo [4/5] Generating Windows resources (icon)...
REM 安装 rsrc 工具（如果需要）
where rsrc >nul 2>nul || (
    echo [INFO] Installing rsrc tool...
    go install github.com/akavel/rsrc@latest
)
REM 生成 syso 文件嵌入图标（使用正确的 manifest）
rsrc -ico build\windows\icon.ico -manifest build\windows\anyproxyai.exe.manifest -o rsrc_windows_amd64.syso 2>nul
if %errorlevel% neq 0 (
    echo [WARN] rsrc with manifest failed, trying without manifest...
    rsrc -ico build\windows\icon.ico -o rsrc_windows_amd64.syso 2>nul
)

echo.
echo [5/5] Building Windows GUI (no console)...
echo.

echo [Windows amd64] Building...
go build -ldflags "-s -w -H windowsgui" -o build\bin\anyproxyai-windows-amd64.exe .
if %errorlevel% equ 0 (echo [OK] Windows amd64) else (echo [FAIL] Windows amd64)

REM 清理 syso 文件
if exist "rsrc_windows_amd64.syso" del /q "rsrc_windows_amd64.syso"

echo.
echo ========================================
echo   Build Complete!
echo ========================================
echo.
echo Output: build\bin\
dir /b build\bin\anyproxyai-windows*.exe 2>nul
echo.
echo ========================================
echo Notes:
echo   - Windows builds: Full GUI with system tray (no console)
echo   - Use 'go run .' for development with console output
echo   - Linux/macOS builds: Use GitHub Actions or build on native platform
echo ========================================
pause
