#!/bin/bash

echo "========================================"
echo "  AnyProxyAi - Build Script"
echo "========================================"

GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

CURRENT_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
[[ "$CURRENT_OS" == darwin* ]] && CURRENT_OS="darwin"
[[ "$CURRENT_OS" == linux* ]] && CURRENT_OS="linux"

echo -e "${BLUE}Current OS:${NC} $CURRENT_OS"

# Check dependencies
command -v go &>/dev/null || { echo "Go not found"; exit 1; }
command -v node &>/dev/null || { echo "Node.js not found"; exit 1; }
command -v wails &>/dev/null || {
    echo "Installing Wails..."
    go install github.com/wailsapp/wails/v2/cmd/wails@latest
    export PATH=$PATH:$(go env GOPATH)/bin
}

# Clean
echo -e "${BLUE}[0/4]${NC} Cleaning..."
rm -f build/bin/anyproxyai-${CURRENT_OS}-*
mkdir -p build/bin

echo -e "${BLUE}[1/4]${NC} Frontend dependencies..."
cd frontend && npm install && cd ..

echo -e "${BLUE}[2/4]${NC} Building frontend..."
cd frontend && npm run build && cd ..

echo -e "${BLUE}[3/4]${NC} Go dependencies..."
go mod tidy && go mod download

echo -e "${BLUE}[4/4]${NC} Building for $CURRENT_OS..."

if [ "$CURRENT_OS" == "linux" ]; then
    # Install Linux dependencies if needed
    if ! dpkg -l | grep -q libgtk-3-dev; then
        echo "Installing GTK dependencies..."
        sudo apt-get update
        sudo apt-get install -y libgtk-3-dev libwebkit2gtk-4.0-dev
    fi
    
    wails build -platform linux/amd64 -o anyproxyai-linux-amd64 && \
        echo -e "${GREEN}[OK]${NC} Linux amd64"
    wails build -platform linux/arm64 -o anyproxyai-linux-arm64 && \
        echo -e "${GREEN}[OK]${NC} Linux arm64"
        
elif [ "$CURRENT_OS" == "darwin" ]; then
    wails build -platform darwin/amd64 -o anyproxyai-darwin-amd64 && \
        echo -e "${GREEN}[OK]${NC} macOS amd64"
    wails build -platform darwin/arm64 -o anyproxyai-darwin-arm64 && \
        echo -e "${GREEN}[OK]${NC} macOS arm64"
fi

chmod +x build/bin/anyproxyai-* 2>/dev/null

echo ""
echo "========================================"
echo "  Build Complete!"
echo "========================================"
ls -la build/bin/anyproxyai-* 2>/dev/null
echo ""
echo "Notes:"
echo "  - This script builds for current platform only"
echo "  - For all platforms, use GitHub Actions"
echo "  - See .github/workflows/build.yml"
echo "========================================"
