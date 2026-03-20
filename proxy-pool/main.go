package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

func main() {
	// Linux 自动设置 ulimit
	setUlimit()

	// 加载配置（热配置容器）
	hotCfg := NewHotConfig("config.json")
	cfg := hotCfg.Get()

	// 初始化结构化日志
	initLogger(false) // true 切换为 JSON 格式

	slog.Info("Proxy Pool Engine 启动",
		"os", runtime.GOOS+"/"+runtime.GOARCH,
		"listen", cfg.ListenAddr,
		"urls_file", cfg.URLsFile,
		"verify_method", cfg.VerifyMethod,
		"concurrency", cfg.Concurrency,
		"timeout_sec", cfg.TimeoutSec,
		"refresh_min", cfg.RefreshIntervalMin,
		"db", cfg.DBPath,
		"decay_alpha", cfg.ScoreDecayAlpha,
		"max_latency_ms", cfg.MaxLatencyMs,
		"blacklist_threshold", cfg.BlacklistFailThreshold,
	)

	// 初始化 GeoIP
	if err := initGeoIP(cfg.GeoIPPath); err != nil {
		slog.Warn("GeoIP 未启用", "error", err)
	} else {
		slog.Info("GeoIP 已加载", "path", cfg.GeoIPPath)
		defer closeGeoIP()
	}

	// 创建数据层（SQLite）
	store, err := NewProxyStore(cfg)
	if err != nil {
		slog.Error("数据库初始化失败", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	slog.Info("数据库已打开", "path", cfg.DBPath, "proxies", store.Total())

	// 首次启动：将 URLS.JSON 迁移到数据库
	store.MigrateURLsFromFile(cfg.URLsFile)
	slog.Info("代理源已加载", "count", store.SourceURLCount())

	// 创建调度器（使用热配置）
	progressCh := make(chan Progress, 50)
	sched := NewScheduler(hotCfg, store, progressCh)

	// 注册路由（无 Web UI，纯 API）
	mux := http.NewServeMux()
	registerRoutes(mux, store, sched, hotCfg)

	// 启动 HTTP 服务
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	// 启动统计快照循环（10 秒刷新，避免 SSE 频繁查库）
	store.StartSnapshotLoop(10 * time.Second)

	// 启动调度器
	sched.Start()

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("引擎 HTTP 服务已启动", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("服务启动失败", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("正在安全退出...")
	sched.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	slog.Info("已退出")
}

// setUlimit 在各平台独立文件中实现（ulimit_linux.go / ulimit_other.go）
