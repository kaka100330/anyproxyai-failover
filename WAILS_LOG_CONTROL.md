# OpenAI Router - Wails 日志控制说明

## 问题说明

Wails v3 框架会输出详细的绑定调用日志：
```
Jan 12 14:47:58.238 INF Binding call started: id=...
Jan 12 14:47:58.241 INF Binding call complete: id=...
Jan 12 14:47:58.238 INF Asset Request: windowName=...
```

这些日志在开发阶段有用，但在生产环境会产生大量输出。

## 解决方案

### 方法 1: 使用生产构建（推荐）

构建生产版本时，Wails 会自动减少日志输出：

```powershell
# 构建生产版本
wails build

# 或使用 Go 构建
go build -ldflags "-s -w" -o anyproxyai.exe .
```

### 方法 2: 设置环境变量

在运行时设置环境变量来控制日志级别：

```powershell
# Windows PowerShell
$env:WAILS_LOGLEVEL="error"
go run .

# 或运行已编译的程序
$env:WAILS_LOGLEVEL="error"
.\anyproxyai.exe
```

```bash
# Linux/macOS
WAILS_LOGLEVEL=error go run .
```

可用的日志级别：
- `trace` - 所有日志（最详细）
- `debug` - 调试信息
- `info` - 信息日志（默认）
- `warning` - 警告信息
- `error` - 仅错误信息
- `fatal` - 仅致命错误

### 方法 3: 创建启动脚本

创建 `run-quiet.ps1`:

```powershell
# run-quiet.ps1
$env:WAILS_LOGLEVEL="error"
go run .
```

或 `run-quiet.bat`:

```batch
@echo off
set WAILS_LOGLEVEL=error
go run .
```

### 方法 4: 在配置文件中添加日志控制（待实现）

未来可以在 `config.json` 中添加：

```json
{
  "debug": false,
  "wailsLogLevel": "error"
}
```

然后在代码中根据配置设置环境变量。

## 当前状态

目前最简单的方式是：

1. **开发时**: 直接使用 `go run .`（查看所有日志）
2. **生产时**: 使用 `wails build` 构建（自动优化日志）
3. **临时需要**: 使用环境变量 `$env:WAILS_LOGLEVEL="error"`

## 注意事项

- Wails 框架日志（`INF Binding call`）无法通过应用代码完全禁用
- 这是 Wails v3 的预期行为，用于调试前后端通信
- 生产构建会自动优化这些日志输出
- 应用自己的日志（`time="..." level=info msg="..."`）可以通过 logrus 配置控制
