package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
	"openai-router-go/internal/service"

	"github.com/gin-gonic/gin"
)

func newTestHandler(t *testing.T) (*gin.Engine, *config.Config) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	db, err := database.InitDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{
		Host:             "127.0.0.1",
		Port:             5642,
		DatabasePath:     filepath.Join(dir, "test.db"),
		WebRoot:          filepath.Join(dir, "web"),
		AdminUsername:    "admin",
		AdminPassword:    "secret-pass",
		SessionSecret:    strings.Repeat("a", 64),
		PublicBaseURL:    "http://127.0.0.1:5642",
		SanitizeThinking: true,
	}
	rs := service.NewRouteService(db)
	ps := service.NewProxyService(rs, cfg)
	r := gin.New()
	NewHandler(NewService(rs, ps, cfg), cfg).Register(r)
	return r, cfg
}

func doJSON(r *gin.Engine, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	r, _ := newTestHandler(t)
	w := doJSON(r, http.MethodPost, "/admin/api/login", `{"username":"admin","password":"wrong"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestLoginAndRPC(t *testing.T) {
	r, _ := newTestHandler(t)

	login := doJSON(r, http.MethodPost, "/admin/api/login", `{"username":"admin","password":"secret-pass"}`, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	session := cookies[0]

	unauth := doJSON(r, http.MethodPost, "/admin/api/rpc", `{"method":"GetStats","args":[]}`, nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rpc status = %d, want 401", unauth.Code)
	}

	w := doJSON(r, http.MethodPost, "/admin/api/rpc", `{"method":"GetStats","args":[]}`, session)
	if w.Code != http.StatusOK {
		t.Fatalf("rpc status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool                   `json:"ok"`
		Result map[string]interface{} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Result == nil {
		t.Fatalf("unexpected rpc response: %s", w.Body.String())
	}
}

func TestUnknownRPCMethod(t *testing.T) {
	r, _ := newTestHandler(t)
	login := doJSON(r, http.MethodPost, "/admin/api/login", `{"username":"admin","password":"secret-pass"}`, nil)
	session := login.Result().Cookies()[0]

	w := doJSON(r, http.MethodPost, "/admin/api/rpc", `{"method":"Shutdown","args":[]}`, session)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
