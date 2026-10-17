# Design Document: Wails v3 Migration

## Overview

本设计文档描述了将 AnyProxyAi 从 Wails v2 迁移到 Wails v3 的技术方案。主要目标是使用 Wails v3 原生系统托盘功能替换有 bug 的第三方托盘库，同时保持所有现有功能不变。

### Migration Goals

1. 使用 Wails v3 原生 `app.SystemTray.New()` API 替换 `energye/systray`
2. 将服务注册方式从 Wails v2 的 `Bind` 改为 Wails v3 的 `Services`
3. 更新窗口管理使用 Wails v3 的 `app.Window.NewWithOptions()`
4. 保持 HTTP API 服务器、适配器、数据库等核心功能完全不变

## Architecture

### Current Architecture (Wails v2)

```
┌─────────────────────────────────────────────────────────────┐
│                        main.go                               │
│  ┌─────────────────┐  ┌─────────────────┐                   │
│  │   Wails v2 App  │  │  energye/systray│ ← 第三方托盘(有bug)│
│  │   (wails.Run)   │  │  (goroutine)    │                   │
│  └────────┬────────┘  └─────────────────┘                   │
│           │                                                  │
│  ┌────────▼────────┐                                        │
│  │   App struct    │ ← Bind to frontend                     │
│  │  (Wails绑定)    │                                        │
│  └────────┬────────┘                                        │
│           │                                                  │
└───────────┼─────────────────────────────────────────────────┘
            │
┌───────────▼─────────────────────────────────────────────────┐
│                    internal/                                 │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐       │
│  │ adapters │ │ config   │ │ database │ │ router   │       │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘       │
│  ┌──────────┐ ┌──────────┐                                  │
│  │ service  │ │ system   │ ← systray_windows.go (复杂)      │
│  └──────────┘ └──────────┘                                  │
└─────────────────────────────────────────────────────────────┘
```

### Target Architecture (Wails v3)

```
┌─────────────────────────────────────────────────────────────┐
│                        main.go                               │
│  ┌─────────────────────────────────────────────────────┐    │
│  │              Wails v3 Application                    │    │
│  │  application.New(application.Options{                │    │
│  │    Services: []application.Service{...},             │    │
│  │  })                                                  │    │
│  └────────┬────────────────────────────────────────────┘    │
│           │                                                  │
│  ┌────────▼────────┐  ┌─────────────────┐                   │
│  │  Main Window    │  │  System Tray    │ ← Wails v3 原生   │
│  │  (WebviewWindow)│  │  (app.SystemTray│                   │
│  └─────────────────┘  │   .New())       │                   │
│                       └─────────────────┘                   │
└─────────────────────────────────────────────────────────────┘
            │
┌───────────▼─────────────────────────────────────────────────┐
│                    internal/                                 │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐       │
│  │ adapters │ │ config   │ │ database │ │ router   │       │
│  │ (不变)   │ │ (不变)   │ │ (不变)   │ │ (不变)   │       │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘       │
│  ┌──────────┐ ┌──────────┐                                  │
│  │ service  │ │ system   │ ← 简化: 只保留 autostart         │
│  │ (不变)   │ │ (简化)   │                                  │
│  └──────────┘ └──────────┘                                  │
└─────────────────────────────────────────────────────────────┘
```

## Components and Interfaces

### 1. Main Application (main.go)

重写 main.go 使用 Wails v3 API：

```go
package main

import (
    "embed"
    "github.com/wailsapp/wails/v3/pkg/application"
    "github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed assets/icon.png assets/icon-dark.png
var trayIcons embed.FS

func main() {
    // 初始化服务 (与现有逻辑相同)
    cfg := config.LoadConfig()
    db, _ := database.InitDB(cfg.DatabasePath)
    routeService := service.NewRouteService(db)
    proxyService := service.NewProxyService(routeService, cfg)
    
    // 创建 Wails v3 应用
    app := application.New(application.Options{
        Name:        "AnyProxyAi",
        Description: "Universal AI API Proxy Router",
        Services: []application.Service{
            application.NewService(&AppService{...}),
        },
        Assets: application.AssetOptions{
            Handler: application.AssetFileServerFS(assets),
        },
    })
    
    // 创建主窗口
    mainWindow := app.Window.NewWithOptions(...)
    
    // 创建系统托盘 (Wails v3 原生)
    systray := app.SystemTray.New()
    systray.SetIcon(iconData)
    systray.SetMenu(trayMenu)
    systray.OnClick(func() { mainWindow.Show() })
    
    app.Run()
}
```

### 2. AppService (服务绑定)

将现有 App struct 改造为 Wails v3 Service：

```go
type AppService struct {
    app          *application.App
    routeService *service.RouteService
    proxyService *service.ProxyService
    config       *config.Config
    autoStart    *system.AutoStart
}

// 所有现有方法保持不变
func (a *AppService) GetRoutes() ([]map[string]interface{}, error)
func (a *AppService) AddRoute(...) error
func (a *AppService) UpdateRoute(...) error
func (a *AppService) DeleteRoute(...) error
func (a *AppService) GetStats() (map[string]interface{}, error)
func (a *AppService) GetConfig() map[string]interface{}
// ... 其他方法
```

### 3. System Tray (简化)

使用 Wails v3 原生托盘 API，删除复杂的第三方库代码：

```go
// 创建托盘
systray := app.SystemTray.New()
systray.SetTooltip("AnyProxyAi")
systray.SetIcon(loadTrayIcon("assets/icon.png"))
systray.SetDarkModeIcon(loadTrayIcon("assets/icon-dark.png"))

// 创建菜单
trayMenu := application.NewMenu()
trayMenu.Add("显示主窗口").OnClick(func(ctx *application.Context) {
    showMainWindow(true)
})
trayMenu.Add("退出").OnClick(func(ctx *application.Context) {
    app.Quit()
})
systray.SetMenu(trayMenu)

// 点击事件
systray.OnClick(func() {
    showMainWindow(true)
})
```

### 4. Window Management

使用 Wails v3 窗口管理：

```go
mainWindow := app.Window.NewWithOptions(application.WebviewWindowOptions{
    Title:     "AnyProxyAi Manager",
    Width:     1280,
    Height:    800,
    MinWidth:  600,
    MinHeight: 300,
    BackgroundColour: application.NewRGB(27, 38, 54),
    URL:              "/",
})

// 窗口关闭时最小化到托盘
mainWindow.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
    if config.MinimizeToTray {
        mainWindow.Hide()
        e.Cancel()
    }
})
```

### 5. HTTP API Server (不变)

HTTP API 服务器完全保持不变，继续使用 Gin 框架：

```go
go func() {
    gin.SetMode(gin.ReleaseMode)
    r := router.SetupAPIRouter(cfg, routeService, proxyService)
    addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
    r.Run(addr)
}()
```

## Data Models

数据模型完全不变，继续使用现有的：

- `config.Config` - 应用配置
- `database.ModelRoute` - 路由配置
- `service.RouteService` - 路由服务
- `service.ProxyService` - 代理服务

## Correctness Properties

*A property is a characteristic or behavior that should hold true across all valid executions of a system-essentially, a formal statement about what the system should do. Properties serve as the bridge between human-readable specifications and machine-verifiable correctness guarantees.*

### Property 1: Service method invocation consistency
*For any* service method call from the frontend, the corresponding Go method should be executed and return the same result format as the Wails v2 version.
**Validates: Requirements 3.3**

### Property 2: API routing consistency
*For any* HTTP request to /api/v1/*, /api/anthropic/*, /api/claudecode/*, or /api/gemini/*, the system should route it to the correct handler and process it with the appropriate format.
**Validates: Requirements 4.2, 4.3, 4.4, 4.5**

### Property 3: Auto-start toggle consistency
*For any* auto-start enable/disable operation, the system should correctly add or remove the platform-specific auto-start configuration.
**Validates: Requirements 5.4**

### Property 4: Configuration round-trip
*For any* valid configuration, saving and then loading should produce an equivalent configuration object.
**Validates: Requirements 6.1**

### Property 5: Route data format consistency
*For any* route query, the returned data should have the same structure (id, name, model, api_url, api_key, group, format, enabled, created, updated) as the Wails v2 version.
**Validates: Requirements 6.3**

### Property 6: Statistics data format consistency
*For any* statistics query, the returned data should have the same structure (route_count, model_count, total_requests, total_tokens, today_requests, today_tokens, success_rate) as the Wails v2 version.
**Validates: Requirements 6.4**

## Error Handling

### Application Startup Errors

1. **Port Already In Use**: 显示错误对话框并退出
2. **Database Initialization Failed**: 记录错误日志并退出
3. **Configuration Load Failed**: 使用默认配置继续

### Runtime Errors

1. **API Request Errors**: 返回适当的 HTTP 错误码和错误消息
2. **Service Method Errors**: 返回错误给前端，由前端显示

## Testing Strategy

### Dual Testing Approach

本项目采用单元测试和属性测试相结合的方式：

- **单元测试**: 验证具体示例和边界情况
- **属性测试**: 验证通用属性在所有输入上都成立

### Property-Based Testing Library

使用 Go 的 `testing/quick` 包进行属性测试。

### Test Categories

1. **Service Method Tests**: 测试 AppService 的所有方法
2. **Configuration Tests**: 测试配置加载和保存
3. **Route Data Format Tests**: 测试路由数据格式一致性
4. **Statistics Format Tests**: 测试统计数据格式一致性

### Test Annotations

每个属性测试必须使用以下格式注释：
```go
// **Feature: wails-v3-migration, Property {number}: {property_text}**
// **Validates: Requirements X.Y**
```

## File Changes Summary

### Files to Create

1. `assets/icon.png` - 托盘图标 (浅色模式)
2. `assets/icon-dark.png` - 托盘图标 (深色模式)
3. `build/config.yml` - Wails v3 构建配置

### Files to Modify

1. `main.go` - 重写使用 Wails v3 API
2. `go.mod` - 更新依赖为 Wails v3

### Files to Delete

1. `internal/system/systray_windows.go` - 删除第三方托盘实现
2. `internal/system/systray_stub.go` - 删除托盘存根

### Files to Keep Unchanged

1. `internal/adapters/*` - 所有适配器
2. `internal/config/*` - 配置管理
3. `internal/database/*` - 数据库
4. `internal/router/*` - HTTP 路由
5. `internal/service/*` - 业务服务
6. `internal/system/autostart_*.go` - 自启动功能
7. `internal/system/dialog_*.go` - 对话框功能
8. `frontend/*` - 前端代码
