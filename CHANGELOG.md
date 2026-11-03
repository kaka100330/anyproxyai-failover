# 更新日志 / Changelog

## v2.5.0

### 修复

- **思维链泄漏（think-as-content）的完整修复**：v2.3.0 只识别正文开头的 `<thinking>`，真实会话里还会出现 `<think>`、`<analysis>`、未闭合标签、正文中的孤立 `</thinking>`，以及 reasoning 通道里的 DSML 闭合残片。这些内容会被客户端当成正文或下一轮上下文，模型随后不断复制自己的标签，出现「思考嵌套」和「思考当正文」并最终中断。
  - 响应侧同时清理 `content` 与 `reasoning_content`：识别 `<thinking>` / `<think>` / `<analysis>`（含未闭合），把正文中的思维链搬回 reasoning；清除两个通道里残留的闭合标签、DSML 闭合残片、`<acp>` / `<control>` 传输标记，以及 reasoning 行首不断增长的 Markdown 引用前缀 `>`。
  - 请求侧在转发前清理 assistant 历史中的 `reasoning_content` / `reasoning` / `reasoning_text`，并移除只含标签残片的空 assistant 消息，避免模型反复模仿上一轮的畸形输出。
  - 流式路径增加分块边界保护，标签被 SSE 分片切成两半时不会被漏出。
- **孤立 DSML 闭合残片不再误判为 `THINK_AS_CONTENT`**：只有包含真实 `invoke` 但无法解析的 DSML 才判为失败；只有 `</parameter></invoke></calls>` 这类闭合残片时记为 `dsml_orphan_tail_dropped`，按空响应处理并允许换端点，而不是直接中断会话。
- **sanitizer 判定失败时按端点故障转移**：此前只在同一端点重试；现在会标记该端点失败并切换到分组内的下一个端点。错误码区分 `THINK_AS_CONTENT`、`EMPTY_UPSTREAM`、`SANITIZE_PARTIAL`，客户端能区分「思维链当正文」「空响应」「已输出部分内容后失败」。
- 新增 `internal/sanitize/normalize.go` 与对应单元测试，覆盖 `<think>` / `<analysis>`、孤立闭合标签、DSML 闭合残片、历史清理和 sanitizer 故障转移。

## v2.3.0

### 新增

- **响应净化（Think 泄漏修复）**：部分 OpenAI 兼容上游会把模型的思考内容当正文返回，或把 DeepSeek 原生工具调用写成 `<｜｜DSML｜｜ invoke ...>` 文本。这类响应对 Agent 客户端是致命的——正文是思维链、`tool_calls` 为空、`finish_reason=stop`，客户端会把半截推理当成最终答案，一轮任务就此中断。
  - 代理会在转发前识别并修复三种形态：`content` 与 `reasoning_content` 逐字相同（丢弃重复正文）、正文被 `<thinking>` 标签包裹（搬回 `reasoning_content`）、正文里残留 DSML 工具调用（解析回真实 `tool_calls` 并把 `finish_reason` 置为 `tool_calls`）。
  - 流式与非流式都支持；修好的内容不会重复下发思考过程。
  - 仅当确认异常时才会重试一次（默认 `maxRetries=1` 的等价行为），正常响应原样透传、不做任何改写，不增加额外延迟。
  - 若重试后仍异常，返回 `THINK_AS_CONTENT` 错误而不是伪造一个答案，避免静默的错误收尾。
  - 开关：设置页「修复思维链泄漏（Think as content）」，配置文件 `sanitize_thinking`，默认开启。
- 新增包 `internal/sanitize`（增量状态机 + 非流式重写 + SSE 重写）及 15 个单元测试。

### 修复

- **全端点冷却后兜底成功却不解除冷却**：当分组内所有端点都在冷却时，代理会按优先级兜底重试；此前兜底一旦成功，该端点的冷却标记仍然保留，而每次请求又会把冷却计时器重置为 N 秒，于是所有端点长期显示「冷却中」，却每个请求都在被调用。
  - 现在只要兜底重试成功，就立即清除该成功端点的冷却（仅此场景），其余仍然失败的端点保持冷却。
  - 新增单元测试 `TestFallbackSuccessClearsCooldown` 覆盖「全部冷却 → 兜底命中恢复端点 → 冷却解除 → 后续请求不再走兜底」。

## v2.2.0

### 修复

- **冷却计时器不再滑动重置**：分组收到请求时，未把已冷却端点的计时器重置为 N 秒。
  - 现在每收到一次请求，就把该分组所有冷却中端点的计时器重置为 N 秒；只有分组静默超过 N 秒后，冷却才解除。
  - 同时修复冷却标记永不到期的问题：旧实现中失败标记只在「分组空闲超过 N 秒」时才被清除，因此持续有流量时坏端点会被永久跳过；现在计时器随请求滑动，静默到期后自动恢复。
  - 界面状态与调度逻辑统一：分组静默超过 N 秒后，端点状态不再显示「冷却中」。

### 新增

- **分组策略**：新建 / 编辑分组时可选
  - `failover` **故障转移**（默认）：按优先级从高到低尝试，失败才降级；
  - `balanced` **均衡负载**：每次请求在可用端点中随机选择，失败顺延；
  - `polling` **轮询**：按优先级顺序轮流分配，失败顺延。
  - 三种策略共用同一套冷却 / 降级 / 全冷却兜底逻辑。
- 数据库 `failover_groups` 新增 `strategy` 列，旧库启动时自动补齐并默认 `failover`。
- 分组卡片新增策略标签，端点管理弹窗显示当前策略。
- 新增单元测试 `internal/service/failover_test.go`：冷却计时滑动重置、静默到期、三种策略排序、策略持久化与旧库迁移。

## v2.1.0

- 首个 Failover Edition 版本：故障转移分组（优先级 + 冷却）、并发路由修复、多格式转换保留。
