package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ==================== 中间件 ====================

// withAPIKey 已移除 — 对外 API 鉴权由 Python 网关统一处理

func withInternalKey(hotCfg *HotConfig, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ik := hotCfg.Get().InternalKey
		if ik != "" {
			key := r.Header.Get("X-Internal-Key")
			if key == "" {
				key = r.URL.Query().Get("internal_key")
			}
			if key != ik {
				http.Error(w, `{"error":"unauthorized: invalid internal key"}`, http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Internal-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// ==================== 路由注册 ====================

func registerRoutes(mux *http.ServeMux, store *ProxyStore, sched *Scheduler, hotCfg *HotConfig) {
	// Go 引擎不暴露任何对外 API，所有 /api/* 由 Python 网关提供
	// 只注册 /internal/* 管理接口（InternalKey 鉴权）

	internal := func(handler http.HandlerFunc) http.HandlerFunc {
		return withCORS(withInternalKey(hotCfg, handler))
	}

	// 配置管理
	mux.HandleFunc("/internal/config", internal(func(w http.ResponseWriter, r *http.Request) {
		handleInternalConfig(w, r, hotCfg, sched)
	}))

	// 引擎状态
	mux.HandleFunc("/internal/stats", internal(func(w http.ResponseWriter, r *http.Request) {
		handleInternalStats(w, r, store, sched)
	}))

	// 任务控制
	mux.HandleFunc("/internal/task/trigger", internal(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sched.IsRunning() {
			writeJSON(w, map[string]string{"status": "already_running"})
			return
		}
		go sched.Trigger()
		writeJSON(w, map[string]string{"status": "triggered"})
	}))
	mux.HandleFunc("/internal/task/cancel", internal(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sched.Cancel()
		writeJSON(w, map[string]string{"status": "cancelled"})
	}))
	mux.HandleFunc("/internal/task/status", internal(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sched.GetProgress())
	}))

	// 代理管理
	mux.HandleFunc("/internal/proxies", internal(func(w http.ResponseWriter, r *http.Request) {
		handleGetProxies(w, r, store)
	}))
	mux.HandleFunc("/internal/proxies/blacklist", internal(func(w http.ResponseWriter, r *http.Request) {
		handleBlacklist(w, r, store)
	}))

	// URL 源管理
	mux.HandleFunc("/internal/urls", internal(func(w http.ResponseWriter, r *http.Request) {
		handleURLs(w, r, hotCfg, store)
	}))

	// 代理源质量统计
	mux.HandleFunc("/internal/sources", internal(func(w http.ResponseWriter, r *http.Request) {
		sources := store.SourceStats()
		writeJSON(w, map[string]interface{}{"sources": sources})
	}))

	// 历史趋势数据
	mux.HandleFunc("/internal/stats/daily", internal(func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if d := r.URL.Query().Get("days"); d != "" {
			if v, err := strconv.Atoi(d); err == nil && v > 0 {
				days = v
			}
		}
		writeJSON(w, map[string]interface{}{"stats": store.GetDailyStats(days)})
	}))

	// Telegram 测试发送
	mux.HandleFunc("/internal/tg/test", internal(func(w http.ResponseWriter, r *http.Request) {
		cfg := hotCfg.Get()
		if cfg.TGBotToken == "" || cfg.TGChatID == "" {
			writeJSON(w, map[string]string{"error": "请先配置 Bot Token 和 Chat ID"})
			return
		}
		err := SendTelegram(cfg.TGBotToken, cfg.TGChatID, "Proxy Pool 测试消息 - 连接正常!")
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, map[string]string{"status": "ok"})
		}
	}))

	// SSE 进度（内部用）
	mux.HandleFunc("/internal/progress", internal(func(w http.ResponseWriter, r *http.Request) {
		sseHandler(w, r, sched)
	}))
}

// ==================== 对外 API 处理 ====================

func handleGetProxies(w http.ResponseWriter, r *http.Request, store *ProxyStore) {
	q := r.URL.Query()
	country := strings.TrimSpace(q.Get("country"))
	protocol := strings.TrimSpace(q.Get("protocol"))
	format := strings.TrimSpace(q.Get("format"))
	sort := strings.TrimSpace(q.Get("sort"))
	minScoreStr := strings.TrimSpace(q.Get("min_score"))
	maxLatStr := strings.TrimSpace(q.Get("max_latency"))
	pageStr := strings.TrimSpace(q.Get("page"))
	sizeStr := strings.TrimSpace(q.Get("size"))
	numberStr := strings.TrimSpace(q.Get("number")) // number 参数（含 "all"）
	ipType := strings.TrimSpace(q.Get("ip_type"))

	opts := QueryOpts{Protocol: protocol, IPType: ipType, Sort: sort}

	if country != "" {
		for _, c := range strings.Split(country, ",") {
			c = strings.ToUpper(strings.TrimSpace(c))
			if c != "" {
				opts.Countries = append(opts.Countries, c)
			}
		}
	}
	if minScoreStr != "" {
		if v, err := strconv.ParseFloat(minScoreStr, 64); err == nil {
			opts.MinScore = v
		}
	}
	if maxLatStr != "" {
		if v, err := strconv.Atoi(maxLatStr); err == nil {
			opts.MaxLatency = v
		}
	}

	// number 参数优先于 page/size
	if numberStr == "all" {
		opts.Page = 1
		opts.Size = 100000 // 实际上取全部
	} else if numberStr != "" {
		if v, err := strconv.Atoi(numberStr); err == nil && v > 0 {
			opts.Page = 1
			opts.Size = v
		}
	} else {
		if pageStr != "" {
			if v, err := strconv.Atoi(pageStr); err == nil && v > 0 {
				opts.Page = v
			}
		}
		if sizeStr != "" {
			if v, err := strconv.Atoi(sizeStr); err == nil && v > 0 {
				opts.Size = v
			}
		}
	}

	entries, total := store.GetAll(opts)

	if format == "json" {
		writeJSON(w, map[string]interface{}{
			"count": len(entries), "total": total,
			"page": opts.Page, "size": opts.Size,
			"proxies": entries,
		})
		return
	}

	// 纯文本：一行一个裸地址
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\n", e.Addr)
	}
}

func handleRandom(w http.ResponseWriter, r *http.Request, store *ProxyStore) {
	country := strings.TrimSpace(r.URL.Query().Get("country"))
	var countries []string
	if country != "" {
		for _, c := range strings.Split(country, ",") {
			c = strings.ToUpper(strings.TrimSpace(c))
			if c != "" {
				countries = append(countries, c)
			}
		}
	}
	entry, ok := store.GetRandom(countries)
	if !ok {
		http.Error(w, "no proxy available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "http://%s", entry.Addr)
}

func handleStats(w http.ResponseWriter, r *http.Request, store *ProxyStore) {
	stats, lastUpdate := store.Stats(20)
	writeJSON(w, map[string]interface{}{
		"total":             store.Total(),
		"avg_score":         store.AvgScore(),
		"blacklisted_count": store.BlacklistedCount(),
		"last_update":       lastUpdate.Format(time.RFC3339),
		"countries":         stats,
	})
}

// ==================== 内部管理 API ====================

func handleInternalConfig(w http.ResponseWriter, r *http.Request, hotCfg *HotConfig, sched *Scheduler) {
	switch r.Method {
	case http.MethodGet:
		cfg := hotCfg.Get()
		// 隐藏敏感字段
		cfg.InternalKey = "***"
		writeJSON(w, cfg)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
			return
		}
		var patch map[string]interface{}
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}

		newCfg, err := hotCfg.Update(patch)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}

		// 通知 scheduler 重置 ticker
		sched.ResetTicker()

		newCfg.InternalKey = "***"
		writeJSON(w, map[string]interface{}{
			"status": "updated",
			"config": newCfg,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleInternalStats(w http.ResponseWriter, r *http.Request, store *ProxyStore, sched *Scheduler) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	snap := GetSnapshot()
	stats, lastUpdate := store.Stats(0)
	writeJSON(w, map[string]interface{}{
		"total_proxies":     snap.TotalProxies,
		"https_count":       snap.HTTPSCount,
		"blacklisted_count": snap.BlacklistedCount,
		"avg_score":         snap.AvgScore,
		"avg_latency":       snap.AvgLatency,
		"idc_count":         snap.IDCCount,
		"isp_count":         snap.ISPCount,
		"unknown_type":      snap.UnknownType,
		"today_new":         snap.TodayNew,
		"last_update":       lastUpdate.Format(time.RFC3339),
		"country_stats":     stats,
		"task":              sched.GetProgress(),
		"runtime": map[string]interface{}{
			"os":              runtime.GOOS,
			"arch":            runtime.GOARCH,
			"goroutines":      runtime.NumGoroutine(),
			"memory_alloc_mb": float64(m.Alloc) / 1024 / 1024,
			"memory_sys_mb":   float64(m.Sys) / 1024 / 1024,
			"go_version":      runtime.Version(),
		},
	})
}

func handleBlacklist(w http.ResponseWriter, r *http.Request, store *ProxyStore) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req struct {
		Addrs  []string `json:"addrs"`
		Action string   `json:"action"` // "blacklist" or "revive"
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	switch req.Action {
	case "revive":
		n := store.ReviveBlacklisted()
		writeJSON(w, map[string]interface{}{"status": "revived", "count": n})
	default:
		// 拉黑指定地址
		for _, addr := range req.Addrs {
			store.BlacklistAddr(addr)
		}
		writeJSON(w, map[string]interface{}{"status": "blacklisted", "count": len(req.Addrs)})
	}
}

func handleURLs(w http.ResponseWriter, r *http.Request, hotCfg *HotConfig, store *ProxyStore) {
	switch r.Method {
	case http.MethodGet:
		urls := store.ListSourceURLs()
		if urls == nil {
			urls = []string{}
		}
		writeJSON(w, map[string]interface{}{"count": len(urls), "urls": urls})

	case http.MethodPut:
		// 全量替换：先删除所有，再添加
		body, _ := io.ReadAll(r.Body)
		var req struct {
			URLs []string `json:"urls"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		// 清空旧的
		store.DeleteSourceURLs(store.ListSourceURLs())
		n := store.AddSourceURLs(req.URLs)
		writeJSON(w, map[string]interface{}{"status": "updated", "count": n})

	case http.MethodPost:
		// 批量添加
		body, _ := io.ReadAll(r.Body)
		var req struct {
			URLs []string `json:"urls"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		n := store.AddSourceURLs(req.URLs)
		writeJSON(w, map[string]interface{}{"status": "added", "count": n})

	case http.MethodDelete:
		// 批量删除
		body, _ := io.ReadAll(r.Body)
		var req struct {
			URLs []string `json:"urls"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		n := store.DeleteSourceURLs(req.URLs)
		writeJSON(w, map[string]interface{}{"status": "deleted", "count": n})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ==================== SSE ====================

func sseHandler(w http.ResponseWriter, r *http.Request, sched *Scheduler) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			p := sched.GetProgress()
			data, _ := json.Marshal(p)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// ==================== 辅助 ====================

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
