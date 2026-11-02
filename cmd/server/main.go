// Command server 以纯 Go 方式运行 AnyProxyAI：代理 API + Web 管理界面。
// 它不依赖 Wails / GTK / WebKit，可直接部署在 Ubuntu 服务器或容器中。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
	"openai-router-go/internal/router"
	"openai-router-go/internal/service"
	"openai-router-go/internal/webapi"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func main() {
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	log.SetLevel(log.InfoLevel)

	cfg := config.LoadConfig()

	if cfg.AdminPassword == "" {
		log.Fatal("admin password is empty; set ANYPROXY_ADMIN_PASSWORD")
	}

	db, err := database.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	routeService := service.NewRouteService(db)
	proxyService := service.NewProxyService(routeService, cfg)

	gin.SetMode(gin.ReleaseMode)
	r := router.SetupAPIRouter(cfg, routeService, proxyService)

	var restartOnce sync.Once
	restartCh := make(chan struct{})
	webService := webapi.NewService(routeService, proxyService, cfg)
	webService.Restart = func() {
		restartOnce.Do(func() { close(restartCh) })
	}
	webapi.NewHandler(webService, cfg).Register(r)

	// OpenAI 兼容根路径：在进入 Gin 路由前把 /v1/... 改写为 /api/v1/...。
	// 这样客户端 base_url 配成 http://host:3000/v1 即可，无需改路径。
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1" || strings.HasPrefix(req.URL.Path, "/v1/") {
			req.URL.Path = "/api" + req.URL.Path
		}
		r.ServeHTTP(w, req)
	})

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Infof("AnyProxyAI server listening on http://%s", addr)
		log.Infof("Web console: http://%s/ (user: %s)", addr, cfg.AdminUsername)
		log.Infof("Proxy base URL: http://%s/v1  (also /api/v1)", addr)
		log.Infof("Database: %s | WebRoot: %s", cfg.DatabasePath, cfg.WebRoot)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Infof("Received signal %s, shutting down", sig)
	case <-restartCh:
		log.Info("Restart requested from web console, shutting down for supervisor restart")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("Graceful shutdown failed: %v", err)
	}
	log.Info("Server stopped")
}
