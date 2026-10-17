# Requirements Document

## Introduction

本文档定义了将 AnyProxyAi 项目从 Wails v2 迁移到 Wails v3 的需求规范。主要目标是解决当前第三方系统托盘库的 bug 问题，使用 Wails v3 原生托盘功能实现更稳定的系统托盘支持，同时保持所有现有功能不变。

## Glossary

- **AnyProxyAi**: 通用 AI API 代理路由器，支持多种 API 格式转换
- **Wails v3**: Go 语言的跨平台桌面应用框架的最新版本
- **System Tray**: 系统托盘，Windows 任务栏右下角的图标区域
- **Adapter**: API 格式转换适配器，用于在不同 AI API 格式之间转换
- **Route**: 路由配置，定义模型到后端 API 的映射关系
- **Proxy Service**: 代理服务，负责请求转发和格式转换

## Requirements

### Requirement 1

**User Story:** As a developer, I want to migrate the project from Wails v2 to Wails v3, so that I can use the native system tray functionality and eliminate third-party library bugs.

#### Acceptance Criteria

1. WHEN the application starts THEN the system SHALL initialize using Wails v3 application framework
2. WHEN the Wails v3 application is created THEN the system SHALL register all existing services (RouteService, ProxyService, Config) as Wails v3 services
3. WHEN the application runs THEN the system SHALL maintain all existing API proxy functionality unchanged
4. WHEN the application builds THEN the system SHALL use Wails v3 build configuration (build/config.yml)

### Requirement 2

**User Story:** As a user, I want the system tray to work reliably on Windows, so that I can minimize the application to tray and access it quickly.

#### Acceptance Criteria

1. WHEN the application starts THEN the system SHALL display a system tray icon using Wails v3 native tray API
2. WHEN the user clicks the tray icon THEN the system SHALL show the main window
3. WHEN the user right-clicks the tray icon THEN the system SHALL display a context menu with "显示主窗口" and "退出" options
4. WHEN the user selects "显示主窗口" from tray menu THEN the system SHALL bring the main window to foreground
5. WHEN the user selects "退出" from tray menu THEN the system SHALL quit the application completely
6. WHEN the user closes the main window (with minimize to tray enabled) THEN the system SHALL hide the window instead of quitting

### Requirement 3

**User Story:** As a user, I want the main window to behave the same as before, so that I can manage routes and view statistics.

#### Acceptance Criteria

1. WHEN the main window opens THEN the system SHALL display with dimensions 1280x800 and minimum size 600x300
2. WHEN the main window loads THEN the system SHALL serve the Vue 3 frontend from embedded assets
3. WHEN the frontend calls backend methods THEN the system SHALL execute the corresponding Go service methods
4. WHEN the window close button is clicked (with minimize to tray enabled) THEN the system SHALL hide the window to tray

### Requirement 4

**User Story:** As a user, I want all existing API proxy features to work unchanged, so that my applications continue to work with the proxy.

#### Acceptance Criteria

1. WHEN the application starts THEN the system SHALL start the HTTP API server on the configured port (default 8080)
2. WHEN a request arrives at /api/v1/* THEN the system SHALL process it as OpenAI format
3. WHEN a request arrives at /api/anthropic/* THEN the system SHALL process it as Claude format
4. WHEN a request arrives at /api/claudecode/* THEN the system SHALL process it as Claude Code format
5. WHEN a request arrives at /api/gemini/* THEN the system SHALL process it as Gemini format
6. WHEN format conversion is needed THEN the system SHALL use the existing adapter system unchanged

### Requirement 5

**User Story:** As a user, I want the auto-start feature to work on all platforms, so that the application starts automatically when I log in.

#### Acceptance Criteria

1. WHEN auto-start is enabled on Windows THEN the system SHALL add a registry entry to HKCU\Software\Microsoft\Windows\CurrentVersion\Run
2. WHEN auto-start is enabled on macOS THEN the system SHALL create a LaunchAgent plist file
3. WHEN auto-start is enabled on Linux THEN the system SHALL create a .desktop file in autostart directory
4. WHEN auto-start is disabled THEN the system SHALL remove the corresponding auto-start configuration

### Requirement 6

**User Story:** As a user, I want the configuration and database to remain compatible, so that I don't lose my existing routes and settings.

#### Acceptance Criteria

1. WHEN the application starts THEN the system SHALL load configuration from config.json in the same format
2. WHEN the application starts THEN the system SHALL connect to the existing SQLite database (routes.db)
3. WHEN routes are queried THEN the system SHALL return data in the same format as before
4. WHEN statistics are queried THEN the system SHALL return data in the same format as before

### Requirement 7

**User Story:** As a developer, I want the project structure to follow Wails v3 conventions, so that the codebase is maintainable.

#### Acceptance Criteria

1. WHEN the project is structured THEN the system SHALL have a build/config.yml file for Wails v3 configuration
2. WHEN the project is structured THEN the system SHALL embed frontend assets using Go embed directive
3. WHEN the project is structured THEN the system SHALL embed tray icons using Go embed directive
4. WHEN services are defined THEN the system SHALL implement them as Wails v3 compatible service structs
