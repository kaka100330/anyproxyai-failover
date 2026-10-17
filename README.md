# AnyProxyAi · Failover Edition

**把「分组」变成真正的故障转移分组，并让同一端点可以并发承载多个会话。**

本仓库是 [cniu6/anyproxyai](https://github.com/cniu6/anyproxyai) v2.0.7 的增强分支。上游原有的多格式转换（OpenAI / Claude / Gemini / Claude Code / Cursor）、本地统一网关、用量统计全部保留，在此基础上重写了两块能力：

| 改动 | 说明 |
| --- | --- |
| ⚡ **故障转移分组** | 原来的「分组」只用于折叠展示路由，现在是一个有序的端点池：按优先级依次尝试，失败自动降级到下一个端点 |
| 🔀 **并发路由** | 同一端点 / 重定向被多个会话同时使用时不再串行阻塞，SQLite 并发写不再丢日志 |

---

## 1. 故障转移分组

### 概念

一个故障转移分组 = **一个对外模型 ID** + **N 个上游端点**（API URL + 密钥 + 格式），端点之间用 `priority` 排序（数字小的优先）。

客户端只需要请求分组暴露的那个模型 ID，网关负责挑选端点、失败降级：

```
client ── model: deepseek-v4.1-flash ──▶ ┌ 优先级 0  官方直连      ── 200 → 返回
                                          ├ 优先级 1  中转 A（401） ── 失败 → 冷却，试下一个
                                          └ 优先级 2  中转 B（超时）── 失败 → 冷却，试下一个
```

### 触发故障转移的条件

| 上游响应 | 行为 |
| --- | --- |
| 网络错误 / 连接失败 / 请求超时 | ✅ 标记端点失败，切换下一个 |
| HTTP `5xx` | ✅ 标记端点失败，切换下一个 |
| HTTP `401` / `403` | ✅ 标记端点失败，切换下一个（典型：密钥失效、额度被禁） |
| HTTP `429` | ❌ 直接返回给客户端（限流是瞬时的，切换会放大请求量） |
| 其他 `4xx`（如 `400` 参数错误） | ❌ 直接返回给客户端（是请求本身的问题，换端点也没用） |

### 冷却规则

- 冷却时长是**分组级**配置（默认 `600` 秒）。
- 端点失败后进入冷却，冷却期内被跳过。
- 冷却计时是**滑动**的：分组每收到一次请求就把计时器重置为 N 秒；只有分组**静默超过 N 秒**后，才清除失败标记、回到最高优先级。
  - 效果：持续有流量时，坏端点一直被跳过，不会每隔 N 秒被重新试探一次。
- 如果所有端点都在冷却，分组按优先级顺序整体兜底重试一次，不会直接报「无可用端点」。
- 编辑分组 / 端点、调整优先级后冷却状态立即清空。

### 重定向

设置页的 `proxy_auto`（或自定义关键字）可以重定向到：

- **故障转移分组** —— 走完整的优先级 + 冷却流程；或
- **单个未分组路由** —— 保持原有行为。

### 未分组路由

未分组端点完全保持上游逻辑：同一模型下多个端点随机负载均衡，没有优先级，也没有冷却。

---

## 2. 并发

原来的实现里，代理路径本身是并发的（每个请求独立 goroutine），真正的问题出在旁路的 SQLite 写入：多个会话同时写请求日志时互相抢锁，出现 `database is locked (SQLITE_BUSY)`，日志丢失，写操作互相等待。

本版修复：

- SQLite 打开 `WAL` 日志模式 + `busy_timeout=10000` + `synchronous=NORMAL`；
- 连接池 `MaxOpenConns=32` / `MaxIdleConns=16`；
- 上游 HTTP `Transport` 不再限制单 host 连接数（`MaxConnsPerHost=0`、`MaxIdleConnsPerHost=100`、`MaxIdleConns=200`），连接充分复用；
- 去掉每个请求的全量 body 日志（全局日志锁 + 大块 IO 会拖慢并发）。

实测：同一分组 / 同一端点，3 路流式请求并发，总耗时等于单路耗时（而不是 3 倍）。

---

## 快速开始

### 下载

到 [Releases](https://github.com/kaka100330/anyproxyai-failover/releases) 下载 `anyproxyai-windows-amd64.exe`，双击运行。托盘常驻，默认监听 `http://127.0.0.1:5642`。

> 首次运行会在 exe 同目录生成 `config.json` 与 `routes.db`，升级时保留这两个文件即可。

### 配置一个故障转移分组

1. 「模型列表」→ **新建分组**：填分组名称、对外模型 ID（例如 `deepseek-v4.1-flash`）、冷却秒数。
2. 点分组卡片上的 **端点** → **添加端点**：填名称、API URL、API Key、API 格式；保存后按优先级排序，可上下调整。
3. 客户端把 `model` 设成该分组的模型 ID 即可，无需感知具体端点。
4. 想让 `proxy_auto` 指向这个分组：分组卡片 → **设为重定向目标**。

### 本地接口

| 协议 | 路径 |
| --- | --- |
| OpenAI | `POST /v1/chat/completions`、`/v1/completions`、`/v1/embeddings`、`/v1/images/generations`、`/v1/audio/*` |
| OpenAI 兼容 | `GET /v1/models` |
| Anthropic | `POST /anthropic/v1/messages` |
| Claude Code | `POST /claude-code/v1/messages` |
| Gemini | `POST /gemini/v1beta/models/:model` |
| Cursor | `POST /cursor/v1/chat/completions` |
| 健康检查 | `GET /health` |

---

## 从源码构建

依赖：Go 1.22+、Node.js 18+。

```bash
cd frontend && npm install && npm run build && cd ..

# Windows（嵌入图标与 manifest，无控制台窗口）
go install github.com/akavel/rsrc@latest
rsrc -ico build/windows/icon.ico -manifest build/windows/anyproxyai.exe.manifest -o rsrc_windows_amd64.syso
go build -ldflags "-s -w -H windowsgui" -o build/bin/anyproxyai-windows-amd64.exe .
```

`main.go` 用 `//go:embed all:frontend/dist` 把前端打进二进制，所以**必须先构建前端**，否则 `go build` 会报 `pattern all:frontend/dist: no matching files found`。

开发调试：`cd frontend && npm run dev` 起 Vite，再 `go run .`。

---

## 代码结构

```
internal/
├── adapters/              # 各协议适配与互转（openai / anthropic / gemini）
├── config/                # 配置读写（含 redirect_target_group_id）
├── database/              # SQLite（WAL、连接池、failover_groups 表、旧分组迁移）
├── router/                # Gin 路由
└── service/
    ├── proxy_service.go       # 12 个代理入口
    ├── proxy_failover.go      # 目标解析 + doWithFailover
    ├── failover_service.go    # 分组 / 端点 CRUD + 冷却状态机
    └── route_service.go       # 路由 CRUD
frontend/src/
├── App.vue                          # 分组卡片 + 未分组路由表
├── components/FailoverGroupModal.vue
└── components/FailoverEndpointsModal.vue
```

数据库变更：`model_routes` 增加 `failover_group_id`、`priority`；新增 `failover_groups(id, name, model, cooldown_seconds, enabled, ...)`。旧版本的 `group` 字段在启动时自动迁移成分组（同名字段的路由归入同一个故障转移分组）。

---

## 上游与许可

- 上游项目：[cniu6/anyproxyai](https://github.com/cniu6/anyproxyai)（MIT）
- 本分支源码：[kaka100330/anyproxyai-failover](https://github.com/kaka100330/anyproxyai-failover)
- 许可证：[MIT](LICENSE)，原始版权归上游作者所有。

界面沿用上游的 Wails v3 + Vue 3 + Naive UI 技术栈：中英文双语、深浅色主题、托盘常驻。
