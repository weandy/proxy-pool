package main

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// TaskStatus 任务状态
type TaskStatus int32

const (
	TaskIdle    TaskStatus = 0
	TaskRunning TaskStatus = 1
)

// Progress 当前任务进度
type Progress struct {
	Running   bool    `json:"running"`
	Done      int64   `json:"done"`
	Total     int64   `json:"total"`
	Alive     int64   `json:"alive"`
	PctDone   float64 `json:"pct_done"`
	StartedAt string  `json:"started_at"`
	Phase     string  `json:"phase"` // "fetch" / "verify"
}

// Scheduler 调度器（支持配置热加载）
type Scheduler struct {
	hotCfg      *HotConfig
	store       *ProxyStore
	progressCh  chan Progress
	batchWriter *BatchWriter // P1-4 批量写入器

	statusFlag atomic.Int32
	roundCount int

	mu        sync.Mutex
	cancelFn  context.CancelFunc
	startedAt time.Time
	progress  Progress

	ticker *time.Ticker
	stopCh chan struct{}
}

// NewScheduler 创建调度器
func NewScheduler(hotCfg *HotConfig, store *ProxyStore, progressCh chan Progress) *Scheduler {
	return &Scheduler{
		hotCfg:      hotCfg,
		store:       store,
		progressCh:  progressCh,
		batchWriter: NewBatchWriter(store, 2000, 100, 500*time.Millisecond),
		stopCh:      make(chan struct{}),
	}
}

// Start 启动定时调度（非阻塞）
func (s *Scheduler) Start() {
	cfg := s.hotCfg.Get()
	interval := time.Duration(cfg.RefreshIntervalMin) * time.Minute
	s.ticker = time.NewTicker(interval)

	go s.Trigger()

	go func() {
		for {
			select {
			case <-s.ticker.C:
				go s.Trigger()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止调度器
func (s *Scheduler) Stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.stopCh)
	s.Cancel()
	if s.batchWriter != nil {
		s.batchWriter.Stop()
	}
}

// ResetTicker 热更新刷新间隔后重置 ticker
func (s *Scheduler) ResetTicker() {
	cfg := s.hotCfg.Get()
	if s.ticker != nil {
		s.ticker.Reset(time.Duration(cfg.RefreshIntervalMin) * time.Minute)
	}
	slog.Info("刷新间隔已更新", "minutes", cfg.RefreshIntervalMin)
}

// Trigger 触发一次完整的增量验证任务
func (s *Scheduler) Trigger() bool {
	if !s.statusFlag.CompareAndSwap(int32(TaskIdle), int32(TaskRunning)) {
		return false
	}
	defer s.statusFlag.Store(int32(TaskIdle))

	cfg := s.hotCfg.Get() // 每次触发时读取最新配置

	s.roundCount++
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelFn = cancel
	s.startedAt = time.Now()
	s.mu.Unlock()
	defer cancel()

	s.report(Progress{Running: true, Phase: "fetch", StartedAt: s.startedAt.Format("2006-01-02 15:04:05")})
	slog.Info("开始抓取代理源", "concurrency", cfg.Concurrency, "timeout_sec", cfg.TimeoutSec)

	// 1. 加载 URL 列表（优先从 DB，fallback 到文件）
	urls := s.store.ListSourceURLs()
	if len(urls) == 0 {
		// DB 中无 URL，尝试从文件加载并迁移
		fileURLs, err := loadURLsFromFile(cfg.URLsFile)
		if err != nil {
			slog.Error("加载 URL 失败", "error", err)
			s.report(Progress{Running: false, Phase: "idle"})
			return false
		}
		s.store.AddSourceURLs(fileURLs)
		urls = fileURLs
		slog.Info("已从文件迁移 URL 到数据库", "count", len(urls))
	}

	// 2. 并发抓取新代理
	rawProxies, sourceMap, fetchResults := fetchAllProxies(urls)
	slog.Info("抓取完成", "raw_proxies", len(rawProxies))

	// 记录每源抓取日志
	for _, fr := range fetchResults {
		s.store.LogSourceFetch(fr.URL, fr.RawCount, fr.NewCount, fr.FetchOK, fr.FetchErr)
	}

	// 3. 蜜罐过滤
	rawProxies = filterHoneypots(rawProxies, cfg.HoneypotThreshold)
	slog.Info("蜜罐过滤完成", "remaining", len(rawProxies))

	// 4. 区分新代理和已有代理
	existingAddrs := s.store.GetAllAddrs()
	existingSet := make(map[string]struct{})
	for _, p := range existingAddrs {
		existingSet[p] = struct{}{}
	}

	var newCandidates []string         // 未入库的新代理
	newSourceMap := make(map[string]string) // 新代理 addr → source URL
	newCountryMap := make(map[string]string) // 新代理 addr → country
	for _, p := range rawProxies {
		if _, exists := existingSet[p]; !exists {
			newCandidates = append(newCandidates, p)
			newSourceMap[p] = sourceMap[p]
			host, _, _ := net.SplitHostPort(p)
			if host != "" {
				newCountryMap[p] = getCountryCode(host)
			} else {
				newCountryMap[p] = "XX"
			}
		}
	}
	slog.Info("代理分类完成", "new", len(newCandidates), "existing", len(existingAddrs))

	// 5. 合并待验证列表（已有代理 + 新候选代理）
	mergeSet := make(map[string]struct{})
	for _, p := range newCandidates {
		mergeSet[p] = struct{}{}
	}
	for _, p := range existingAddrs {
		mergeSet[p] = struct{}{}
	}

	// 6. 黑名单复活
	if cfg.BlacklistReviveRounds > 0 && s.roundCount%cfg.BlacklistReviveRounds == 0 {
		n := s.store.ReviveBlacklisted()
		if n > 0 {
			slog.Info("复活黑名单代理", "count", n)
			for _, p := range s.store.GetAllAddrs() {
				mergeSet[p] = struct{}{}
			}
		}
	}

	var toVerify []string
	for p := range mergeSet {
		toVerify = append(toVerify, p)
	}

	// 构建新代理集合（用于在回调中区分）
	newSet := make(map[string]struct{})
	for _, p := range newCandidates {
		newSet[p] = struct{}{}
	}

	total := int64(len(toVerify))
	s.report(Progress{Running: true, Phase: "verify", Total: total, StartedAt: s.startedAt.Format("2006-01-02 15:04:05")})
	slog.Info("开始 HTTP 验证", "count", len(toVerify), "concurrency", cfg.Concurrency)

	// 7. HTTP 验证（已有代理走 BatchWriter，新代理记录结果）
	var httpPassed []string
	var httpPassedMu sync.Mutex
	var newPassed []string  // 验证通过的新代理
	var newPassedMu sync.Mutex

	RunVerifyPipeline(ctx, cfg, toVerify, func(r CheckResult) {
		if _, isNew := newSet[r.Addr]; isNew {
			// 新代理：不走 BatchWriter，只记录通过的
			if r.Ok {
				newPassedMu.Lock()
				newPassed = append(newPassed, r.Addr)
				newPassedMu.Unlock()
			}
			// 不通过的新代理直接丢弃，不入库
		} else {
			// 已有代理：走 BatchWriter 更新验证结果
			s.batchWriter.Submit(CheckItem{
				Addr:               r.Addr,
				Ok:                 r.Ok,
				LatencyMs:          r.LatencyMs,
				Alpha:              cfg.ScoreDecayAlpha,
				MaxLatMs:           cfg.MaxLatencyMs,
				BlacklistThreshold: cfg.BlacklistFailThreshold,
			})
		}
		if r.Ok {
			httpPassedMu.Lock()
			httpPassed = append(httpPassed, r.Addr)
			httpPassedMu.Unlock()
		}
		pct := 0.0
		if r.Total > 0 {
			pct = float64(r.Done) / float64(r.Total) * 100
		}
		s.report(Progress{
			Running: true, Phase: "verify",
			Done: r.Done, Total: r.Total, Alive: r.Alive,
			PctDone: pct, StartedAt: s.startedAt.Format("2006-01-02 15:04:05"),
		})
	})

	// 确保已有代理的验证结果全部落盘
	s.batchWriter.Flush()

	// 8. 将验证通过的新代理入库
	for _, p := range newPassed {
		country := newCountryMap[p]
		s.store.Upsert(ProxyEntry{Addr: p, Country: country, Protocol: "http"}, newSourceMap[p])
	}
	slog.Info("HTTP 验证完成", "passed", len(httpPassed), "total", len(toVerify),
		"new_inserted", len(newPassed), "new_discarded", len(newCandidates)-len(newPassed))

	// 8. 第二轮：HTTPS 复测（只验证 HTTP 通过的代理）
	if len(httpPassed) > 0 && ctx.Err() == nil {
		// 非破坏性：先收集通过 HTTPS 的代理，最后批量更新
		var httpsOK []string
		var httpsOKMu sync.Mutex

		countryMap := s.store.GetCountryMap()
		httpsTotal := int64(len(httpPassed))
		s.report(Progress{Running: true, Phase: "verify_https", Total: httpsTotal, StartedAt: s.startedAt.Format("2006-01-02 15:04:05")})
		slog.Info("开始 HTTPS 复测", "count", len(httpPassed), "concurrency", cfg.Concurrency)

		RunVerifyHTTPS(ctx, cfg, httpPassed, countryMap, func(r CheckResult) {
			if r.Ok {
				httpsOKMu.Lock()
				httpsOK = append(httpsOK, r.Addr)
				httpsOKMu.Unlock()
			}
			pct := 0.0
			if r.Total > 0 {
				pct = float64(r.Done) / float64(r.Total) * 100
			}
			s.report(Progress{
				Running: true, Phase: "verify_https",
				Done: r.Done, Total: r.Total, Alive: r.Alive,
				PctDone: pct, StartedAt: s.startedAt.Format("2006-01-02 15:04:05"),
			})
		})

		// 批量更新：先重置所有为 http，再标记通过的为 http,https
		s.store.ResetProtocol()
		for _, addr := range httpsOK {
			s.store.UpdateProtocol(addr, "http,https")
		}
		slog.Info("HTTPS 复测完成", "https", len(httpsOK), "http_only", len(httpPassed)-len(httpsOK))
	}

	s.store.MarkUpdated()
	slog.Info("验证完成", "alive", s.store.Total(), "blacklisted", s.store.BlacklistedCount())

	// 9. IP 类型检测（仅对尚未检测的新代理）
	undetected := s.store.ListUndetectedIPs(1) // 快速检查有没有待检测的
	if len(undetected) > 0 && ctx.Err() == nil {
		s.report(Progress{Running: true, Phase: "ip_detect", StartedAt: s.startedAt.Format("2006-01-02 15:04:05")})
		slog.Info("开始 IP 类型检测")
		DetectIPTypes(s.store)
	}

	// 10. 自动淘汰
	purged := 0
	if cfg.AutoPurgeEnabled && cfg.AutoPurgeRounds > 0 {
		purged = s.store.PurgeDeadProxies(cfg.AutoPurgeRounds)
		if purged > 0 {
			slog.Info("自动淘汰", "purged", purged, "rounds", cfg.AutoPurgeRounds)
		}
	}

	// 11. 记录每日统计
	s.store.RecordDailyStats(purged)

	// 12. 刷新快照
	s.store.RefreshSnapshot()

	// 12.5 每 10 轮 VACUUM 回收磁盘空间（避免频繁锁库）
	if s.roundCount%10 == 0 {
		s.store.Vacuum()
	}

	// 13. Telegram 通知
	NotifyRoundSummary(cfg, s.store, purged)
	CheckLowStockAlert(cfg, s.store.Total())

	s.report(Progress{Running: false, Phase: "idle"})
	return true
}

// Cancel 取消当前任务
func (s *Scheduler) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelFn != nil {
		s.cancelFn()
	}
}

// GetProgress 获取当前进度
func (s *Scheduler) GetProgress() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress
}

// IsRunning 是否有任务进行
func (s *Scheduler) IsRunning() bool {
	return s.statusFlag.Load() == int32(TaskRunning)
}

func (s *Scheduler) report(p Progress) {
	s.mu.Lock()
	s.progress = p
	s.mu.Unlock()
	select {
	case s.progressCh <- p:
	default:
	}
}
