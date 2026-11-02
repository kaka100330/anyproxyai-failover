package webapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"openai-router-go/internal/config"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const sessionCookieName = "anyproxy_admin"
const sessionTTL = 7 * 24 * time.Hour

// Handler 提供 Web 管理界面、登录鉴权与静态资源服务。
type Handler struct {
	Service *Service
	Config  *config.Config
}

func NewHandler(svc *Service, cfg *config.Config) *Handler {
	return &Handler{Service: svc, Config: cfg}
}

// Register 注册管理端路由。代理 API 路由需在此之前注册。
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/login", h.loginPage)
	r.POST("/admin/api/login", h.login)
	r.POST("/admin/api/logout", h.logout)
	r.GET("/admin/api/session", h.session)

	admin := r.Group("/admin/api")
	admin.Use(h.authMiddleware)
	admin.POST("/rpc", h.rpc)

	// 其余路径交给 SPA。
	r.NoRoute(h.serveSPA)
}

func (h *Handler) loginPage(c *gin.Context) {
	if h.isLoggedIn(c) {
		c.Redirect(http.StatusFound, "/")
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, loginHTML)
}

func (h *Handler) login(c *gin.Context) {
	username := c.PostForm("username")
	password := c.PostForm("password")
	isForm := c.ContentType() == "application/x-www-form-urlencoded" || c.ContentType() == "multipart/form-data"

	if !isForm {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&body); err == nil {
			username, password = body.Username, body.Password
		}
	}

	if subtle.ConstantTimeCompare([]byte(username), []byte(h.Config.AdminUsername)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(h.Config.AdminPassword)) != 1 {
		log.Warnf("Failed web login from %s for user %q", c.ClientIP(), username)
		if isForm {
			c.Redirect(http.StatusFound, "/login?error=1")
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "用户名或密码错误"})
		return
	}

	h.setSession(c)
	log.Infof("Web login success from %s", c.ClientIP())
	if isForm {
		c.Redirect(http.StatusFound, "/")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) logout(c *gin.Context) {
	c.SetCookie(sessionCookieName, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) session(c *gin.Context) {
	if !h.isLoggedIn(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": h.Config.AdminUsername})
}

func (h *Handler) authMiddleware(c *gin.Context) {
	if !h.isLoggedIn(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		c.Abort()
		return
	}
	c.Next()
}

func (h *Handler) isLoggedIn(c *gin.Context) bool {
	value, err := c.Cookie(sessionCookieName)
	if err != nil || value == "" {
		return false
	}
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payload, sig := parts[0], parts[1]
	expected := h.sign(payload)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return false
	}
	fields := strings.SplitN(payload, "|", 2)
	if len(fields) != 2 || fields[0] != h.Config.AdminUsername {
		return false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return true
}

func (h *Handler) setSession(c *gin.Context) {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := fmt.Sprintf("%s|%d", h.Config.AdminUsername, exp)
	value := payload + "." + h.sign(payload)
	c.SetCookie(sessionCookieName, value, int(sessionTTL.Seconds()), "/", "", false, true)
}

func (h *Handler) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(h.Config.SessionSecret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// rpc 通过反射把 HTTP 请求分发到 Service 的导出方法，从而复用桌面版
// 已有的前端调用方式（window.go.main.App.Method）。
func (h *Handler) rpc(c *gin.Context) {
	var req struct {
		Method string            `json:"method"`
		Args   []json.RawMessage `json:"args"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request body"})
		return
	}
	if req.Method == "" || strings.HasPrefix(req.Method, "Shutdown") {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid method"})
		return
	}

	method := reflect.ValueOf(h.Service).MethodByName(req.Method)
	if !method.IsValid() {
		c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "unknown method: " + req.Method})
		return
	}
	mt := method.Type()
	if mt.IsVariadic() {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "variadic methods are not supported"})
		return
	}

	args := make([]reflect.Value, mt.NumIn())
	for i := 0; i < mt.NumIn(); i++ {
		argType := mt.In(i)
		value := reflect.New(argType).Elem()
		if i < len(req.Args) && string(req.Args[i]) != "null" {
			if err := json.Unmarshal(req.Args[i], value.Addr().Interface()); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": fmt.Sprintf("argument %d: %v", i, err)})
				return
			}
		}
		args[i] = value
	}

	var result interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("RPC %s panic: %v", req.Method, r)
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": fmt.Sprintf("internal error: %v", r)})
			}
		}()
		outs := method.Call(args)
		if len(outs) > 0 {
			last := outs[len(outs)-1]
			if last.Type().Implements(reflect.TypeOf((*error)(nil)).Elem()) {
				if !last.IsNil() {
					c.JSON(http.StatusOK, gin.H{"ok": false, "error": last.Interface().(error).Error()})
					return
				}
				outs = outs[:len(outs)-1]
			}
		}
		if len(outs) > 0 {
			result = outs[0].Interface()
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "result": result})
	}()
}

// serveSPA 提供前端静态文件；未登录时重定向到登录页。
func (h *Handler) serveSPA(c *gin.Context) {
	if strings.HasPrefix(c.Request.URL.Path, "/admin/") || strings.HasPrefix(c.Request.URL.Path, "/api/") {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if !h.isLoggedIn(c) {
		c.Redirect(http.StatusFound, "/login")
		return
	}

	webRoot := h.Config.WebRoot
	if webRoot == "" {
		webRoot = "web"
	}
	cleanPath := filepath.Clean("/" + c.Request.URL.Path)
	filePath := filepath.Join(webRoot, cleanPath)
	if !strings.HasPrefix(filePath, filepath.Clean(webRoot)+string(os.PathSeparator)) && filePath != filepath.Clean(webRoot) {
		c.Status(http.StatusNotFound)
		return
	}
	if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
		c.File(filePath)
		return
	}
	indexPath := filepath.Join(webRoot, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		c.String(http.StatusNotFound, "Web UI not built. Run: npm run build:web")
		return
	}
	c.File(indexPath)
}

const loginHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AnyProxyAI 管理登录</title>
<style>
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
    font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
    background:linear-gradient(135deg,#0f172a 0%,#1e293b 60%,#0b1220 100%);color:#e2e8f0}
  .card{width:360px;max-width:92vw;background:rgba(15,23,42,.86);border:1px solid rgba(148,163,184,.25);
    border-radius:16px;padding:32px 28px;box-shadow:0 24px 64px rgba(0,0,0,.45)}
  h1{font-size:20px;margin:0 0 6px;font-weight:650}
  p.sub{margin:0 0 24px;color:#94a3b8;font-size:13px}
  label{display:block;font-size:13px;color:#cbd5e1;margin:14px 0 6px}
  input{width:100%;padding:11px 12px;border-radius:9px;border:1px solid #334155;background:#0b1220;color:#e2e8f0;font-size:14px;outline:none}
  input:focus{border-color:#38bdf8;box-shadow:0 0 0 3px rgba(56,189,248,.15)}
  button{width:100%;margin-top:22px;padding:11px;border:0;border-radius:9px;cursor:pointer;
    background:#0284c7;color:#fff;font-size:14px;font-weight:600}
  button:hover{background:#0369a1}
  button:disabled{opacity:.6;cursor:default}
  .err{display:none;margin-top:14px;padding:9px 11px;border-radius:8px;background:rgba(239,68,68,.12);
    border:1px solid rgba(239,68,68,.35);color:#fca5a5;font-size:13px}
</style>
</head>
<body>
<form class="card" id="form">
  <h1>AnyProxyAI</h1>
  <p class="sub">故障转移网关 · 管理控制台</p>
  <label for="u">用户名</label>
  <input id="u" name="username" autocomplete="username" required autofocus>
  <label for="p">密码</label>
  <input id="p" name="password" type="password" autocomplete="current-password" required>
  <button id="btn" type="submit">登录</button>
  <div class="err" id="err"></div>
</form>
<script>
  if (new URLSearchParams(location.search).has('error')) {
    document.getElementById('err').textContent = '用户名或密码错误';
    document.getElementById('err').style.display = 'block';
  }
  document.getElementById('form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = document.getElementById('btn');
    const err = document.getElementById('err');
    btn.disabled = true; err.style.display = 'none';
    try {
      const res = await fetch('/admin/api/login', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          username: document.getElementById('u').value,
          password: document.getElementById('p').value
        })
      });
      if (res.ok) { location.href = '/'; return; }
      const data = await res.json().catch(() => ({}));
      err.textContent = data.error || '登录失败';
      err.style.display = 'block';
    } catch (e2) {
      err.textContent = '网络错误：' + e2.message;
      err.style.display = 'block';
    } finally {
      btn.disabled = false;
    }
  });
</script>
</body>
</html>`
