#!/bin/bash

echo "========================================"
echo "  OpenAI Router - Quick Start"
echo "========================================"
echo ""
echo "Select mode:"
echo "  1. Desktop App (Wails GUI)"
echo "  2. Web Server (API only)"
echo "  3. Development Mode (with hot reload)"
echo ""
read -p "Enter your choice (1-3): " mode

case $mode in
    1)
        echo ""
        echo "Starting Desktop Application..."
        if [ -f "build/bin/OpenAI Router" ]; then
            ./build/bin/OpenAI\ Router
        elif [ -f "build/bin/openai-router" ]; then
            ./build/bin/openai-router
        else
            echo "[ERROR] Desktop app not found. Please run build.sh first."
            exit 1
        fi
        ;;
    2)
        echo ""
        read -p "Enter host (default: localhost): " host
        read -p "Enter port (default: 8000): " port

        host=${host:-localhost}
        port=${port:-8000}

        echo "Starting Web Server at http://$host:$port"
        echo "API Endpoint: http://$host:$port/api"
        echo ""
        echo "Press Ctrl+C to stop the server"
        echo ""

        # Detect OS
        os_name=$(uname -s)
        case "$os_name" in
            Linux*)
                if [ -f "build/bin/openai-router-linux-amd64" ]; then
                    ./build/bin/openai-router-linux-amd64 -web -host "$host" -port "$port"
                else
                    echo "[WARN] Binary not found, running from source..."
                    go run . -web -host "$host" -port "$port"
                fi
                ;;
            Darwin*)
                # Detect architecture
                arch=$(uname -m)
                if [ "$arch" = "arm64" ]; then
                    if [ -f "build/bin/openai-router-darwin-arm64" ]; then
                        ./build/bin/openai-router-darwin-arm64 -web -host "$host" -port "$port"
                    else
                        echo "[WARN] Binary not found, running from source..."
                        go run . -web -host "$host" -port "$port"
                    fi
                else
                    if [ -f "build/bin/openai-router-darwin-amd64" ]; then
                        ./build/bin/openai-router-darwin-amd64 -web -host "$host" -port "$port"
                    else
                        echo "[WARN] Binary not found, running from source..."
                        go run . -web -host "$host" -port "$port"
                    fi
                fi
                ;;
            *)
                echo "[WARN] Unknown OS, running from source..."
                go run . -web -host "$host" -port "$port"
                ;;
        esac
        ;;
    3)
        echo ""
        echo "Starting Development Mode..."
        echo ""

        # Start frontend dev server in background
        cd frontend
        npm run dev &
        frontend_pid=$!
        cd ..

        # Wait for frontend to start
        echo "Waiting for frontend to start..."
        sleep 3

        # Start backend
        echo "Starting backend..."
        go run . -web

        # Cleanup on exit
        trap "kill $frontend_pid 2>/dev/null" EXIT
        ;;
    *)
        echo "Invalid choice!"
        exit 1
        ;;
esac
