# Web 版部署（Ubuntu / Docker / systemd）

Web 版把原来的 Wails 桌面外壳拆掉，变成：

```
浏览器 ── HTTP ──▶ Go 服务（Gin）
                     ├─ /admin/api/*  管理 API（登录 + RPC）
                     ├─ /v1/*         代理 API（OpenAI 兼容）
                     └─ /             前端静态文件（Vue 构建产物）
                                   │
                                   └─ SQLite（routes.db）
```

- 前端：`frontend/`，`npm run build:web` 产出纯静态文件。
- 后端：`cmd/server`，不依赖 Wails / GTK / WebKit，可交叉编译为 linux/amd64 静态二进制。
- 数据库：单文件 SQLite，默认 `/data/routes.db`。
- 管理界面：登录后使用与桌面版完全相同的 Vue 界面；桌面专属的托盘 / 开机自启项在 Web 模式下自动隐藏或为空操作。

## 1. 构建

在开发机（Windows / macOS / Linux 均可）执行：

```bash
VERSION=2.6.0 bash scripts/build-server.sh
```

产物：

```
build/server/
├── anyproxyai-server        # linux/amd64 静态二进制
├── web/                     # 前端静态文件
├── Dockerfile
├── docker-compose.yml
└── ca-certificates.crt
```

> 脚本会先执行 `npm run build:web`，再用 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 编译。
> `ca-certificates.crt` 用于容器内访问上游 HTTPS；Linux 上通常位于 `/etc/ssl/certs/ca-certificates.crt`。

## 2. Docker 部署（推荐）

以下命令在 Ubuntu 服务器上执行。

```bash
# 1) 上传并解压构建产物
mkdir -p /opt/anyproxyai-build
tar -xzf anyproxyai-server-2.6.0-linux-amd64.tar.gz -C /opt/anyproxyai-build

# 2) 确保二进制有执行权限后构建镜像
chmod 755 /opt/anyproxyai-build/anyproxyai-server
cd /opt/anyproxyai-build
docker build -t anyproxyai:2.6.0 .

# 3) 准备数据目录和 compose 文件
mkdir -p /opt/anyproxyai/data
cp docker-compose.yml /opt/anyproxyai/docker-compose.yml
cd /opt/anyproxyai

# 4) 修改 docker-compose.yml 里的环境变量（见下一节），然后启动
docker compose up -d
docker compose ps
docker logs -f anyproxyai
```

默认只发布到 `127.0.0.1:3000`，与原来的 new-api 一致，不会直接暴露到公网。

### 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ANYPROXY_HOST` | `localhost` | 监听地址；容器内必须是 `0.0.0.0` |
| `ANYPROXY_PORT` | `5642` | 监听端口；容器内用 `3000` |
| `ANYPROXY_DB` | `routes.db` | SQLite 路径；容器内用 `/data/routes.db` |
| `ANYPROXY_WEB_ROOT` | `web` | 前端静态文件目录；容器内用 `/app/web` |
| `ANYPROXY_API_KEY` | 空 | 代理 API 的本地密钥；为空则不校验（不要这样部署） |
| `ANYPROXY_ADMIN_USER` | `admin` | 管理界面用户名 |
| `ANYPROXY_ADMIN_PASSWORD` | 随机生成 | 管理界面密码；生产必须显式设置 |
| `ANYPROXY_SESSION_SECRET` | 随机生成 | 登录 Cookie 的 HMAC 密钥；重启后保持不变才能保持登录 |
| `ANYPROXY_PUBLIC_BASE_URL` | `http://127.0.0.1:<port>` | 管理界面里展示的代理地址 |
| `ANYPROXY_SANITIZE_THINKING` | `true` | 是否修复 `think` 内容混入正文的响应 |
| `ANYPROXY_ENABLE_FILE_LOG` | `false` | 是否写文件日志 |

`config.json` 会在工作目录生成（compose 中为 `/data`），保存界面里修改的重定向等配置；环境变量优先级更高。

## 3. systemd 部署（不用 Docker）

```bash
sudo mkdir -p /opt/anyproxyai
sudo tar -xzf anyproxyai-server-2.6.0-linux-amd64.tar.gz -C /opt/anyproxyai
sudo chmod 755 /opt/anyproxyai/anyproxyai-server

sudo cp /opt/anyproxyai/anyproxyai.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now anyproxyai
sudo systemctl status anyproxyai
```

`/opt/anyproxyai/anyproxyai.env` 示例（权限建议 `600`）：

```ini
ANYPROXY_HOST=127.0.0.1
ANYPROXY_PORT=3000
ANYPROXY_DB=/opt/anyproxyai/data/routes.db
ANYPROXY_WEB_ROOT=/opt/anyproxyai/web
ANYPROXY_API_KEY=sk-your-local-key
ANYPROXY_ADMIN_USER=admin
ANYPROXY_ADMIN_PASSWORD=change-me
ANYPROXY_SESSION_SECRET=change-me-to-a-long-random-string
ANYPROXY_PUBLIC_BASE_URL=http://127.0.0.1:3000
```

## 4. 远程访问管理界面

默认监听 `127.0.0.1:3000`，推荐用 SSH 隧道（与原 new-api 的访问方式一致）：

```bash
ssh -L 3000:127.0.0.1:3000 root@<服务器IP>
```

然后在本地浏览器打开 <http://127.0.0.1:3000>，用 `ANYPROXY_ADMIN_USER` / `ANYPROXY_ADMIN_PASSWORD` 登录。

如果确实需要直接公网访问，把 compose 的端口改成 `"0.0.0.0:3000:3000"`，并务必在前置 Nginx / Caddy 上配置 HTTPS 与访问控制；管理界面本身只有密码登录，不应长期裸奔在公网 HTTP 上。

### 完全前后端分离（Nginx 静态托管）

`deploy/nginx.conf.example` 提供了一个 Nginx 配置：Nginx 直接托管 `web/` 静态文件，只把 `/admin/`、`/v1/`、`/api/`、`/login` 反代给 Go 进程。这样静态资源不经过 Go，适合正式环境：

```nginx
root /opt/anyproxyai/web;
location /v1/ { proxy_pass http://127.0.0.1:3000; proxy_buffering off; }
location /admin/ { proxy_pass http://127.0.0.1:3000; }
```

Go 进程仍监听 `127.0.0.1:3000`，由 Nginx 监听公网端口。

## 5. 代理 API

管理界面展示的地址形如 `http://127.0.0.1:3000`。客户端配置：

```
Base URL: http://<host>:3000/v1
API Key:  $ANYPROXY_API_KEY
```

`/v1/...` 与 `/api/v1/...` 等价，因此从 new-api 迁移时只需把 `base_url` 的 host 从 `new-api:3000` 改为 `anyproxyai:3000`，路径不用动。

支持：

- `POST /v1/chat/completions`（OpenAI，含流式）
- `POST /v1/messages`（Anthropic / Claude Code）
- `POST /v1/models`、`GET /v1/models`
- Gemini、Cursor 等原有适配路由（`/api/gemini`、`/api/cursor` 等）
- `GET /health`

## 6. 从 new-api 迁移

1. 在 Web 界面「模型 / 故障转移」里新建分组，分组模型填客户端请求的模型名。
2. 每个 new-api channel 对应一个端点；new-api 的 priority 数值越大越优先，AnyProxyAI 相反（越小越优先），导入时注意顺序。
3. 如果某个端点需要把对外模型映射成上游模型名（new-api 的 `model_mapping`），在该端点的「上游模型」填上游实际模型名；留空则使用分组模型。
4. 代理 API 的本地密钥（`ANYPROXY_API_KEY`）替代 new-api 的 token；下游客户端只改 base_url 和 key。

## 7. 运维

```bash
docker logs -f anyproxyai          # 日志
docker compose restart anyproxyai  # 重启
docker stats anyproxyai            # CPU / 内存
```

数据库备份：直接备份 `/opt/anyproxyai/data/`（SQLite WAL 模式下建议先 `docker compose stop anyproxyai` 再复制）。

管理 API 的 RPC 端点 `POST /admin/api/rpc` 只接受登录后的 Cookie；登录 Cookie 为 HttpOnly、SameSite=Lax，有效期 7 天。
