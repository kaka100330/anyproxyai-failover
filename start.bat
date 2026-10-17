@echo off
echo ========================================
echo   OpenAI Router - Quick Start
echo ========================================
echo.
echo Starting Desktop Application (GUI)...
echo.


REM 查找可执行文件
if exist "build\bin\openai-router.exe" (
    echo Found: build\bin\openai-router.exe
    start "" "build\bin\openai-router.exe"
    echo.
    echo Desktop GUI application started!
    echo The app includes both GUI management interface and API server.
    echo API server will be available at: http://localhost:8000/api
    echo.
    timeout /t 3 /nobreak >nul
    exit /b 0
) else if exist "build\bin\OpenAI Router.exe" (
    echo Found: build\bin\OpenAI Router.exe
    start "" "build\bin\OpenAI Router.exe"
    echo.
    echo Desktop GUI application started!
    echo The app includes both GUI management interface and API server.
    echo API server will be available at: http://localhost:8000/api
    echo.
    timeout /t 3 /nobreak >nul
    exit /b 0
) else (
    echo [ERROR] Desktop app not found!
    echo.
    echo Please build the application first by running:
    echo   build.bat
    echo.
    echo Or run in development mode:
    echo   wails dev
    echo.
    pause
    exit /b 1
)
