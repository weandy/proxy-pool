package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ProxyEntry 单个代理条目
type ProxyEntry struct {
	Addr        string    `json:"addr"`
	Country     string    `json:"country"`
	Protocol    string    `json:"protocol"`
	Latency     int64     `json:"latency"`      // 最近一次延迟(ms)
	AvgLatency  float64   `json:"avg_latency"`  // EMA 平均延迟
	SuccessRate float64   `json:"success_rate"` // EMA 成功率 (0~1)
	CheckCount  int       `json:"check_count"`
	FailCount   int       `json:"fail_count"` // 连续失败次数
	Score       float64   `json:"score"`      // 综合评分 0~100
	Blacklisted bool      `json:"blacklisted"`
	FirstSeen   time.Time `json:"first_seen"`
	LastChecked time.Time `json:"last_checked"`
	LastSuccess time.Time `json:"last_success"`
	IPType      string    `json:"ip_type"` // idc / isp / ""
	ISPName     string    `json:"isp_name"`
	ASInfo      string    `json:"as_info"`
}

// CountryStat 国家统计
type CountryStat struct {
	Country string `json:"country"`
	Count   int    `json:"count"`
}

// ProxyStore SQLite 后端的代理仓库
type ProxyStore struct {
	mu         sync.RWMutex
	db         *sql.DB
	cfg        Config
	lastUpdate time.Time
}

// NewProxyStore 创建基于 SQLite 的代理仓库
func NewProxyStore(cfg Config) (*ProxyStore, error) {
	db, err := sql.Open("sqlite", cfg.DBPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite 单写

	s := &ProxyStore{db: db, cfg: cfg}
	if err := s.initSchema(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ProxyStore) initSchema() error {
	// 基础表
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS proxies (
		addr          TEXT PRIMARY KEY,
		country       TEXT DEFAULT 'XX',
		protocol      TEXT DEFAULT 'http',
		source_url    TEXT DEFAULT '',
		latency       INTEGER DEFAULT 0,
		avg_latency   REAL DEFAULT 0,
		success_rate  REAL DEFAULT 0,
		check_count   INTEGER DEFAULT 0,
		fail_count    INTEGER DEFAULT 0,
		score         REAL DEFAULT 0,
		blacklisted   INTEGER DEFAULT 0,
		first_seen    DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_checked  DATETIME,
		last_success  DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_score ON proxies(score DESC);
	CREATE INDEX IF NOT EXISTS idx_country ON proxies(country);
	CREATE INDEX IF NOT EXISTS idx_blacklisted ON proxies(blacklisted);
	CREATE INDEX IF NOT EXISTS idx_source ON proxies(source_url);

	CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY
	);
	`)
	if err != nil {
		return err
	}

	// #7 版本化迁移
	return s.migrate()
}

// migrate 按版本号执行迁移
func (s *ProxyStore) migrate() error {
	currentVersion := 0
	row := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version")
	row.Scan(&currentVersion)

	migrations := []struct {
		version int
		sql     string
	}{
		{1, "ALTER TABLE proxies ADD COLUMN source_url TEXT DEFAULT ''"},
		{2, `CREATE TABLE IF NOT EXISTS proxy_sources (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			url        TEXT UNIQUE NOT NULL,
			is_active  INTEGER DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`},
		{3, `CREATE TABLE IF NOT EXISTS source_fetch_logs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			source_url TEXT NOT NULL,
			fetched_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			raw_count  INTEGER DEFAULT 0,
			new_count  INTEGER DEFAULT 0,
			dup_count  INTEGER DEFAULT 0,
			fetch_ok   INTEGER DEFAULT 1,
			fetch_error TEXT DEFAULT ''
		)`},
		{4, `ALTER TABLE proxies ADD COLUMN ip_type TEXT DEFAULT ''`},
		{5, `ALTER TABLE proxies ADD COLUMN isp_name TEXT DEFAULT ''`},
		{6, `ALTER TABLE proxies ADD COLUMN as_info TEXT DEFAULT ''`},
		{7, `ALTER TABLE proxies ADD COLUMN ip_risk INTEGER DEFAULT -1`},
		// 复合索引：加速分页查询（WHERE blacklisted=0 AND check_count>0 ORDER BY score DESC）
		{8, `CREATE INDEX IF NOT EXISTS idx_proxy_list ON proxies(blacklisted, check_count, score DESC)`},
		{9, `CREATE INDEX IF NOT EXISTS idx_ip_type ON proxies(ip_type) WHERE ip_type != ''`},
		{10, `CREATE INDEX IF NOT EXISTS idx_proxy_filter ON proxies(blacklisted, check_count, ip_type, score DESC)`},
		// 自动淘汰：追踪连续拉黑轮次
		{11, `ALTER TABLE proxies ADD COLUMN blacklist_rounds INTEGER DEFAULT 0`},
		// 历史趋势：每日统计
		{12, `CREATE TABLE IF NOT EXISTS daily_stats (
			date TEXT PRIMARY KEY,
			alive INTEGER DEFAULT 0,
			new_count INTEGER DEFAULT 0,
			purged INTEGER DEFAULT 0,
			avg_latency REAL DEFAULT 0,
			avg_score REAL DEFAULT 0,
			idc_count INTEGER DEFAULT 0,
			isp_count INTEGER DEFAULT 0
		)`},
	}

	for _, m := range migrations {
		if m.version > currentVersion {
			_, err := s.db.Exec(m.sql)
			if err != nil {
				// 列已存在等非致命错误跳过
				if !isColumnExistsError(err) {
					return fmt.Errorf("迁移 v%d 失败: %w", m.version, err)
				}
			}
			s.db.Exec("INSERT OR REPLACE INTO schema_version (version) VALUES (?)", m.version)
			slog.Info("DB 迁移完成", "version", m.version)
		}
	}

	// #10 启动时清理过期代理
	s.CleanupStale(7)

	// #P1-6 清理过期 source_fetch_logs
	s.CleanupFetchLogs(90)

	// SQLite 优化
	s.db.Exec("PRAGMA optimize")

	return nil
}

// isColumnExistsError 判断是否为"列已存在"错误
func isColumnExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
}

// CleanupStale 清理超过 N 天未成功的代理
func (s *ProxyStore) CleanupStale(days int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(
		`DELETE FROM proxies WHERE last_success IS NOT NULL AND last_success < ? AND blacklisted = 1`,
		cutoff,
	)
	if err != nil {
		slog.Warn("清理过期代理失败", "error", err)
		return
	}
	if cnt, _ := result.RowsAffected(); cnt > 0 {
		slog.Info("已清理过期黑名单代理", "count", cnt, "days", days)
	}
}

// CleanupFetchLogs 清理超过 N 天的 source_fetch_logs
func (s *ProxyStore) CleanupFetchLogs(days int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(`DELETE FROM source_fetch_logs WHERE fetched_at < ?`, cutoff)
	if err != nil {
		return
	}
	if cnt, _ := result.RowsAffected(); cnt > 0 {
		slog.Info("已清理过期抓取日志", "count", cnt, "days", days)
	}
}

// Close 关闭数据库
func (s *ProxyStore) Close() {
	if s.db != nil {
		s.db.Close()
	}
}

// Vacuum 回收 SQLite 磁盘空间
func (s *ProxyStore) Vacuum() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("VACUUM")
	if err != nil {
		slog.Warn("VACUUM 失败", "error", err)
	} else {
		slog.Info("VACUUM 完成")
	}
}

// Upsert 插入或更新代理（首次发现时使用）
func (s *ProxyStore) Upsert(entry ProxyEntry, sourceURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO proxies (addr, country, protocol, source_url, first_seen, last_checked)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(addr) DO UPDATE SET
			country = CASE WHEN excluded.country != 'XX' THEN excluded.country ELSE proxies.country END,
			source_url = CASE WHEN excluded.source_url != '' THEN excluded.source_url ELSE proxies.source_url END
	`, entry.Addr, entry.Country, entry.Protocol, sourceURL)
	if err != nil {
		slog.Warn("Upsert 失败", "addr", entry.Addr, "error", err)
	}
}

// computeScore 根据 EMA 数据计算综合分
func computeScore(successRate, avgLatency float64, maxLatencyMs int) float64 {
	stability := successRate * 100
	speedScore := math.Max(0, 100-avgLatency/float64(maxLatencyMs)*100)
	score := stability*0.7 + speedScore*0.3
	return math.Round(score*10) / 10
}

// UpdateCheck 验证后更新代理的评分数据
func (s *ProxyStore) UpdateCheck(addr string, ok bool, latencyMs int64, alpha float64, maxLatMs int, blacklistThreshold int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 先读取当前状态
	var avgLat, sr float64
	var cc, fc int
	err := s.db.QueryRow(`SELECT avg_latency, success_rate, check_count, fail_count FROM proxies WHERE addr = ?`, addr).
		Scan(&avgLat, &sr, &cc, &fc)
	if err != nil {
		return // 不存在就跳过
	}

	cc++
	now := time.Now()

	if ok {
		lat := float64(latencyMs)
		if cc == 1 {
			// 首次验证，直接赋值
			avgLat = lat
			sr = 1.0
		} else {
			sr = alpha*1.0 + (1-alpha)*sr
			avgLat = alpha*lat + (1-alpha)*avgLat
		}
		fc = 0
		score := computeScore(sr, avgLat, maxLatMs)
		_, _ = s.db.Exec(`
			UPDATE proxies SET
				latency = ?, avg_latency = ?, success_rate = ?,
				check_count = ?, fail_count = 0, score = ?,
				blacklisted = 0, last_checked = ?, last_success = ?
			WHERE addr = ?
		`, latencyMs, avgLat, sr, cc, score, now, now, addr)
	} else {
		sr = alpha*0.0 + (1-alpha)*sr
		fc++
		score := computeScore(sr, avgLat, maxLatMs)
		blacklisted := 0
		if fc >= blacklistThreshold {
			blacklisted = 1
		}
		_, _ = s.db.Exec(`
			UPDATE proxies SET
				success_rate = ?, check_count = ?, fail_count = ?,
				score = ?, blacklisted = ?, last_checked = ?
			WHERE addr = ?
		`, sr, cc, fc, score, blacklisted, now, addr)
	}
}

// GetAll 按条件查询代理列表
type QueryOpts struct {
	Countries  []string
	Protocol   string
	IPType     string  // "idc", "isp", "" (不过滤)
	Sort       string  // "score" (默认) 或 "latency"
	MinScore   float64 // 0 表示不过滤
	MaxLatency int     // 0 表示不过滤 (ms)
	Page       int     // 1-based
	Size       int     // 每页大小，0 表示不分页
}

func (s *ProxyStore) GetAll(opts QueryOpts) ([]ProxyEntry, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	where := "blacklisted = 0 AND check_count > 0 AND success_rate > 0"
	args := []interface{}{}

	if len(opts.Countries) > 0 {
		placeholders := ""
		for i, c := range opts.Countries {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, c)
		}
		where += " AND country IN (" + placeholders + ")"
	}
	if opts.Protocol != "" {
		if opts.Protocol == "https" || opts.Protocol == "http,https" {
			// HTTPS 筛选：只返回支持 HTTPS 的
			where += " AND protocol = 'http,https'"
		}
		// HTTP 筛选不需要额外条件，所有存活代理都支持 HTTP
	}
	if opts.MinScore > 0 {
		where += " AND score >= ?"
		args = append(args, opts.MinScore)
	}
	if opts.MaxLatency > 0 {
		where += " AND avg_latency <= ? AND avg_latency > 0"
		args = append(args, float64(opts.MaxLatency))
	}
	if opts.IPType != "" {
		where += " AND ip_type = ?"
		args = append(args, opts.IPType)
	}

	// 计算总数
	countSQL := "SELECT COUNT(*) FROM proxies WHERE " + where
	var total int
	_ = s.db.QueryRow(countSQL, args...).Scan(&total)

	// 排序
	orderBy := "score DESC"
	if opts.Sort == "latency" {
		orderBy = "avg_latency ASC"
	}

	query := "SELECT addr, country, protocol, latency, avg_latency, success_rate, check_count, fail_count, score, first_seen, last_checked, last_success, ip_type, isp_name, as_info FROM proxies WHERE " + where + " ORDER BY " + orderBy

	// 分页
	if opts.Size > 0 {
		offset := 0
		if opts.Page > 1 {
			offset = (opts.Page - 1) * opts.Size
		}
		query += fmt.Sprintf(" LIMIT %d OFFSET %d", opts.Size, offset)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0
	}
	defer rows.Close()

	var result []ProxyEntry
	for rows.Next() {
		var e ProxyEntry
		var firstSeen, lastChecked, lastSuccess sql.NullTime
		var ipType, ispName, asInfo sql.NullString
		if err := rows.Scan(&e.Addr, &e.Country, &e.Protocol, &e.Latency, &e.AvgLatency, &e.SuccessRate, &e.CheckCount, &e.FailCount, &e.Score, &firstSeen, &lastChecked, &lastSuccess, &ipType, &ispName, &asInfo); err != nil {
			continue
		}
		if firstSeen.Valid {
			e.FirstSeen = firstSeen.Time
		}
		if lastChecked.Valid {
			e.LastChecked = lastChecked.Time
		}
		if lastSuccess.Valid {
			e.LastSuccess = lastSuccess.Time
		}
		if ipType.Valid {
			e.IPType = ipType.String
		}
		if ispName.Valid {
			e.ISPName = ispName.String
		}
		if asInfo.Valid {
			e.ASInfo = asInfo.String
		}
		result = append(result, e)
	}
	return result, total
}

// GetRandom 加权随机选一个代理（SQL 内完成加权，避免全表扫描）
func (s *ProxyStore) GetRandom(countries []string) (ProxyEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	where := "blacklisted = 0 AND check_count > 0 AND success_rate > 0"
	args := []interface{}{}

	if len(countries) > 0 {
		placeholders := ""
		for i, c := range countries {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, c)
		}
		where += " AND country IN (" + placeholders + ")"
	}

	// 加权随机：score 越高被选中概率越大
	query := fmt.Sprintf(`SELECT addr, country, protocol, latency, avg_latency, success_rate,
		check_count, fail_count, score, first_seen, last_checked, last_success,
		ip_type, isp_name, as_info
		FROM proxies WHERE %s
		ORDER BY (MAX(score, 1) * ABS(RANDOM() %% 1000)) DESC LIMIT 1`, where)

	var e ProxyEntry
	var firstSeen, lastChecked, lastSuccess sql.NullTime
	var ipType, ispName, asInfo sql.NullString
	err := s.db.QueryRow(query, args...).Scan(
		&e.Addr, &e.Country, &e.Protocol, &e.Latency, &e.AvgLatency,
		&e.SuccessRate, &e.CheckCount, &e.FailCount, &e.Score,
		&firstSeen, &lastChecked, &lastSuccess, &ipType, &ispName, &asInfo,
	)
	if err != nil {
		return ProxyEntry{}, false
	}
	if firstSeen.Valid {
		e.FirstSeen = firstSeen.Time
	}
	if lastChecked.Valid {
		e.LastChecked = lastChecked.Time
	}
	if lastSuccess.Valid {
		e.LastSuccess = lastSuccess.Time
	}
	if ipType.Valid {
		e.IPType = ipType.String
	}
	if ispName.Valid {
		e.ISPName = ispName.String
	}
	if asInfo.Valid {
		e.ASInfo = asInfo.String
	}
	return e, true
}

// GetAllAddrs 获取所有非黑名单代理地址（用于增量合并）
func (s *ProxyStore) GetAllAddrs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT addr FROM proxies WHERE blacklisted = 0")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var addrs []string
	for rows.Next() {
		var addr string
		if rows.Scan(&addr) == nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// GetBlacklistedAddrs 获取黑名单代理（用于复活）
func (s *ProxyStore) GetBlacklistedAddrs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT addr FROM proxies WHERE blacklisted = 1")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var addrs []string
	for rows.Next() {
		var addr string
		if rows.Scan(&addr) == nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// ReviveBlacklisted 将黑名单代理解除，重新参与验证
func (s *ProxyStore) ReviveBlacklisted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec("UPDATE proxies SET blacklisted = 0, fail_count = 0 WHERE blacklisted = 1")
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// BlacklistAddr 手动拉黑指定代理
func (s *ProxyStore) BlacklistAddr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec("UPDATE proxies SET blacklisted = 1 WHERE addr = ?", addr)
}

// Total 有效代理总数（非黑名单且至少验证通过一次）
func (s *ProxyStore) Total() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE blacklisted = 0 AND check_count > 0 AND success_rate > 0").Scan(&n)
	return n
}

// BlacklistedCount 黑名单数量
func (s *ProxyStore) BlacklistedCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE blacklisted = 1").Scan(&n)
	return n
}

// AvgScore 有效代理平均分
func (s *ProxyStore) AvgScore() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var avg sql.NullFloat64
	_ = s.db.QueryRow("SELECT AVG(score) FROM proxies WHERE blacklisted = 0 AND check_count > 0 AND success_rate > 0").Scan(&avg)
	if avg.Valid {
		return math.Round(avg.Float64*10) / 10
	}
	return 0
}

// Stats 各国统计（Top N）
func (s *ProxyStore) Stats(topN int) ([]CountryStat, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := "SELECT country, COUNT(*) as cnt FROM proxies WHERE blacklisted = 0 AND check_count > 0 AND success_rate > 0 GROUP BY country ORDER BY cnt DESC"
	if topN > 0 {
		query += fmt.Sprintf(" LIMIT %d", topN)
	}

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, s.lastUpdate
	}
	defer rows.Close()

	var stats []CountryStat
	for rows.Next() {
		var cs CountryStat
		if rows.Scan(&cs.Country, &cs.Count) == nil {
			stats = append(stats, cs)
		}
	}
	return stats, s.lastUpdate
}

// MarkUpdated 标记更新时间
func (s *ProxyStore) MarkUpdated() {
	s.mu.Lock()
	s.lastUpdate = time.Now()
	s.mu.Unlock()
}

// FormatAddr 格式化单行输出
func FormatAddr(e ProxyEntry) string {
	return fmt.Sprintf("http://%s", e.Addr)
}

// UpdateProtocol 更新代理协议标记（HTTPS 复测通过后调用）
func (s *ProxyStore) UpdateProtocol(addr string, protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec("UPDATE proxies SET protocol = ? WHERE addr = ?", protocol, addr)
}

// ResetProtocol 每轮验证开始前，重置所有代理为 http（等待 HTTPS 复测再标回）
func (s *ProxyStore) ResetProtocol() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec("UPDATE proxies SET protocol = 'http' WHERE protocol = 'http,https'")
}

// GetCountryMap 获取代理地址→国家代码映射（供 HTTPS 分流使用）
func (s *ProxyStore) GetCountryMap() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT addr, country FROM proxies WHERE blacklisted = 0")
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var addr, country string
		if rows.Scan(&addr, &country) == nil {
			result[addr] = country
		}
	}
	return result
}

// HTTPSCount 支持 HTTPS 的有效代理数量
func (s *ProxyStore) HTTPSCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE blacklisted = 0 AND check_count > 0 AND success_rate > 0 AND protocol = 'http,https'").Scan(&n)
	return n
}

// SourceStat 代理源统计（增强版）
type SourceStat struct {
	URL        string  `json:"url"`
	Total      int     `json:"total"`       // 贡献代理总数
	AliveHTTP  int     `json:"alive_http"`  // HTTP 存活
	AliveHTTPS int     `json:"alive_https"` // HTTPS 存活
	AvgScore   float64 `json:"avg_score"`   // 平均评分
	AvgLatency float64 `json:"avg_latency"` // 平均延迟
	// 以下来自 source_fetch_logs
	TotalFetched     int     `json:"total_fetched"`      // 历史总抓取
	TotalNew         int     `json:"total_new"`          // 历史去重后
	DupRate          float64 `json:"dup_rate"`           // 累计重复率
	LastFetchAt      string  `json:"last_fetch_at"`      // 最后成功抓取时间
	FetchSuccessRate float64 `json:"fetch_success_rate"` // 拉取成功率
}

// SourceStats 按 source_url 聚合统计（联合 fetch_logs）
func (s *ProxyStore) SourceStats() []SourceStat {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 1. 存量统计
	rows, err := s.db.Query(`
		SELECT source_url,
		       COUNT(*) as total,
		       SUM(CASE WHEN blacklisted = 0 AND check_count > 0 AND success_rate > 0 AND protocol = 'http' THEN 1 ELSE 0 END) as alive_http,
		       SUM(CASE WHEN blacklisted = 0 AND check_count > 0 AND success_rate > 0 AND protocol = 'http,https' THEN 1 ELSE 0 END) as alive_https,
		       AVG(CASE WHEN blacklisted = 0 AND check_count > 0 AND success_rate > 0 THEN score ELSE NULL END) as avg_score,
		       AVG(CASE WHEN blacklisted = 0 AND check_count > 0 AND success_rate > 0 THEN avg_latency ELSE NULL END) as avg_lat
		FROM proxies
		WHERE source_url != ''
		GROUP BY source_url
		ORDER BY (alive_http + alive_https) DESC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	statsMap := make(map[string]*SourceStat)
	var result []SourceStat
	for rows.Next() {
		var st SourceStat
		var avgScore, avgLat *float64
		if rows.Scan(&st.URL, &st.Total, &st.AliveHTTP, &st.AliveHTTPS, &avgScore, &avgLat) == nil {
			if avgScore != nil {
				st.AvgScore = *avgScore
			}
			if avgLat != nil {
				st.AvgLatency = *avgLat
			}
			result = append(result, st)
			statsMap[st.URL] = &result[len(result)-1]
		}
	}

	// 2. 抓取日志统计
	logRows, err := s.db.Query(`
		SELECT source_url,
		       SUM(raw_count) as total_fetched,
		       SUM(new_count) as total_new,
		       MAX(CASE WHEN fetch_ok = 1 THEN fetched_at ELSE NULL END) as last_fetch,
		       COUNT(id) as total_fetches,
		       SUM(fetch_ok) as success_fetches
		FROM source_fetch_logs
		GROUP BY source_url
	`)
	if err == nil {
		defer logRows.Close()
		for logRows.Next() {
			var url string
			var lastFetch *string
			var totalFetched, totalNew, totalFetches, successFetches int
			if logRows.Scan(&url, &totalFetched, &totalNew, &lastFetch, &totalFetches, &successFetches) == nil {
				if st, ok := statsMap[url]; ok {
					st.TotalFetched = totalFetched
					st.TotalNew = totalNew
					if lastFetch != nil {
						st.LastFetchAt = *lastFetch
					}
					if totalFetched > 0 {
						st.DupRate = float64(totalFetched-totalNew) / float64(totalFetched) * 100
					}
					if totalFetches > 0 {
						st.FetchSuccessRate = float64(successFetches) / float64(totalFetches) * 100
					}
				}
			}
		}
	}

	return result
}

// ==================== URL 源管理（DB） ====================

// ListSourceURLs 列出所有活跃的源 URL
func (s *ProxyStore) ListSourceURLs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query("SELECT url FROM proxy_sources WHERE is_active = 1 ORDER BY created_at")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			urls = append(urls, u)
		}
	}
	return urls
}

// AddSourceURLs 批量添加源 URL（忽略重复）
func (s *ProxyStore) AddSourceURLs(urls []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := 0
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || (!strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://")) {
			continue
		}
		_, err := s.db.Exec("INSERT OR IGNORE INTO proxy_sources (url) VALUES (?)", u)
		if err == nil {
			added++
		}
	}
	return added
}

// DeleteSourceURLs 批量删除源 URL
func (s *ProxyStore) DeleteSourceURLs(urls []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for _, u := range urls {
		result, err := s.db.Exec("DELETE FROM proxy_sources WHERE url = ?", u)
		if err == nil {
			if n, _ := result.RowsAffected(); n > 0 {
				deleted++
			}
		}
	}
	return deleted
}

// SourceURLCount 源 URL 总数
func (s *ProxyStore) SourceURLCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM proxy_sources WHERE is_active = 1").Scan(&n)
	return n
}

// MigrateURLsFromFile 从 JSON 文件导入 URL 到数据库（首次迁移用）
func (s *ProxyStore) MigrateURLsFromFile(path string) {
	// 仅当数据库中无 URL 时才迁移
	if s.SourceURLCount() > 0 {
		return
	}
	urls, err := loadURLsFromFile(path)
	if err != nil {
		return
	}
	n := s.AddSourceURLs(urls)
	if n > 0 {
		slog.Info("已从文件迁移源 URL 到数据库", "path", path, "count", n)
	}
}

// LogSourceFetch 记录一次源抓取日志
func (s *ProxyStore) LogSourceFetch(sourceURL string, rawCount, newCount int, fetchOK bool, fetchErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := 1
	if !fetchOK {
		ok = 0
	}
	s.db.Exec(
		`INSERT INTO source_fetch_logs (source_url, raw_count, new_count, dup_count, fetch_ok, fetch_error)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sourceURL, rawCount, newCount, rawCount-newCount, ok, fetchErr,
	)
}

// UpdateIPInfo 更新代理的 IP 类型检测结果
func (s *ProxyStore) UpdateIPInfo(addr, ipType, ispName, asInfo string, risk int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(
		`UPDATE proxies SET ip_type = ?, isp_name = ?, as_info = ?, ip_risk = ? WHERE addr = ?`,
		ipType, ispName, asInfo, risk, addr,
	)
}

// ListUndetectedIPs 列出尚未检测 IP 类型的有效代理地址
func (s *ProxyStore) ListUndetectedIPs(limit int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		`SELECT addr FROM proxies WHERE ip_type = '' AND blacklisted = 0 AND check_count > 0 AND success_rate > 0 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var addrs []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// ==================== Dashboard 增强统计 ====================

// IPTypeStats 返回 IDC / ISP / 未知 各自的数量
func (s *ProxyStore) IPTypeStats() (idc, isp, unknown int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE blacklisted=0 AND check_count>0 AND success_rate>0 AND ip_type='idc'`).Scan(&idc)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE blacklisted=0 AND check_count>0 AND success_rate>0 AND ip_type='isp'`).Scan(&isp)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE blacklisted=0 AND check_count>0 AND success_rate>0 AND (ip_type='' OR ip_type IS NULL)`).Scan(&unknown)
	return
}

// AvgLatencyAlive 存活代理平均延迟
func (s *ProxyStore) AvgLatencyAlive() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var avg sql.NullFloat64
	_ = s.db.QueryRow(`SELECT AVG(avg_latency) FROM proxies WHERE blacklisted=0 AND check_count>0 AND success_rate>0 AND avg_latency>0`).Scan(&avg)
	if avg.Valid {
		return math.Round(avg.Float64)
	}
	return 0
}

// TodayNewCount 今日新增代理数
func (s *ProxyStore) TodayNewCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE first_seen >= date('now')`).Scan(&n)
	return n
}

// ==================== 自动淘汰 ====================

// IncrBlacklistRounds 拉黑时增加轮次计数（在 UpdateCheck 拉黑后调用）
func (s *ProxyStore) IncrBlacklistRounds(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`UPDATE proxies SET blacklist_rounds = blacklist_rounds + 1 WHERE addr = ?`, addr)
}

// ResetBlacklistRounds 复活时重置轮次
func (s *ProxyStore) ResetBlacklistRounds() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`UPDATE proxies SET blacklist_rounds = 0 WHERE blacklisted = 0`)
}

// PurgeDeadProxies 删除连续 N 轮仍在黑名单的代理
func (s *ProxyStore) PurgeDeadProxies(maxRounds int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`DELETE FROM proxies WHERE blacklisted = 1 AND blacklist_rounds >= ?`, maxRounds)
	if err != nil {
		return 0
	}
	n, _ := result.RowsAffected()
	return int(n)
}

// ==================== 历史趋势 ====================

// DailyStat 每日统计
type DailyStat struct {
	Date       string  `json:"date"`
	Alive      int     `json:"alive"`
	NewCount   int     `json:"new_count"`
	Purged     int     `json:"purged"`
	AvgLatency float64 `json:"avg_latency"`
	AvgScore   float64 `json:"avg_score"`
	IDCCount   int     `json:"idc_count"`
	ISPCount   int     `json:"isp_count"`
}

// RecordDailyStats 记录/更新今日统计（UPSERT）— 合并为 2 条查询
func (s *ProxyStore) RecordDailyStats(purged int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	today := time.Now().Format("2006-01-02")

	// #P1-7 合并为一条聚合查询（原来 6 条独立查询）
	var alive, idc, isp int
	var avgLat, avgSc sql.NullFloat64
	_ = s.db.QueryRow(`
		SELECT
			COUNT(*),
			AVG(CASE WHEN avg_latency > 0 THEN avg_latency ELSE NULL END),
			AVG(score),
			SUM(CASE WHEN ip_type = 'idc' THEN 1 ELSE 0 END),
			SUM(CASE WHEN ip_type = 'isp' THEN 1 ELSE 0 END)
		FROM proxies
		WHERE blacklisted = 0 AND check_count > 0 AND success_rate > 0
	`).Scan(&alive, &avgLat, &avgSc, &idc, &isp)

	var newCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE first_seen >= date('now')`).Scan(&newCount)

	latVal, scVal := 0.0, 0.0
	if avgLat.Valid {
		latVal = math.Round(avgLat.Float64)
	}
	if avgSc.Valid {
		scVal = math.Round(avgSc.Float64*10) / 10
	}

	s.db.Exec(`INSERT INTO daily_stats (date, alive, new_count, purged, avg_latency, avg_score, idc_count, isp_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(date) DO UPDATE SET alive=?, new_count=?, purged=purged+?, avg_latency=?, avg_score=?, idc_count=?, isp_count=?`,
		today, alive, newCount, purged, latVal, scVal, idc, isp,
		alive, newCount, purged, latVal, scVal, idc, isp,
	)
}

// GetDailyStats 获取近 N 天的历史统计
func (s *ProxyStore) GetDailyStats(days int) []DailyStat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT date, alive, new_count, purged, avg_latency, avg_score, idc_count, isp_count FROM daily_stats ORDER BY date DESC LIMIT ?`, days)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var stats []DailyStat
	for rows.Next() {
		var d DailyStat
		if rows.Scan(&d.Date, &d.Alive, &d.NewCount, &d.Purged, &d.AvgLatency, &d.AvgScore, &d.IDCCount, &d.ISPCount) == nil {
			stats = append(stats, d)
		}
	}
	// 反转为时间正序
	for i, j := 0, len(stats)-1; i < j; i, j = i+1, j-1 {
		stats[i], stats[j] = stats[j], stats[i]
	}
	return stats
}

// ==================== Stats 快照缓存 ====================

// StatsSnapshot 内存统计快照（避免频繁 COUNT 查询）
type StatsSnapshot struct {
	TotalProxies     int     `json:"total_proxies"`
	HTTPSCount       int     `json:"https_count"`
	BlacklistedCount int     `json:"blacklisted_count"`
	AvgScore         float64 `json:"avg_score"`
	AvgLatency       float64 `json:"avg_latency"`
	IDCCount         int     `json:"idc_count"`
	ISPCount         int     `json:"isp_count"`
	UnknownType      int     `json:"unknown_type"`
	TodayNew         int     `json:"today_new"`
	UpdatedAt        string  `json:"updated_at"`
}

// snapshot 内部快照
var snapshotMu sync.RWMutex
var snapshot StatsSnapshot

// RefreshSnapshot 刷新统计快照
func (s *ProxyStore) RefreshSnapshot() {
	snap := StatsSnapshot{
		TotalProxies:     s.Total(),
		HTTPSCount:       s.HTTPSCount(),
		BlacklistedCount: s.BlacklistedCount(),
		AvgScore:         s.AvgScore(),
		AvgLatency:       s.AvgLatencyAlive(),
		TodayNew:         s.TodayNewCount(),
		UpdatedAt:        time.Now().Format(time.RFC3339),
	}
	snap.IDCCount, snap.ISPCount, snap.UnknownType = s.IPTypeStats()

	snapshotMu.Lock()
	snapshot = snap
	snapshotMu.Unlock()
}

// GetSnapshot 获取当前快照
func GetSnapshot() StatsSnapshot {
	snapshotMu.RLock()
	defer snapshotMu.RUnlock()
	return snapshot
}

// StartSnapshotLoop 启动快照刷新循环
func (s *ProxyStore) StartSnapshotLoop(interval time.Duration) {
	s.RefreshSnapshot() // 立即刷新一次
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			s.RefreshSnapshot()
		}
	}()
}

// ==================== P1-4: BatchWriter 批量写入 ====================

// CheckItem 一条待批量写入的验证结果
type CheckItem struct {
	Addr               string
	Ok                 bool
	LatencyMs          int64
	Alpha              float64
	MaxLatMs           int
	BlacklistThreshold int
}

// BatchWriter 批量写入验证结果（减少锁竞争 + 事务合并）
type BatchWriter struct {
	store    *ProxyStore
	ch       chan CheckItem
	stopCh   chan struct{}
	doneCh   chan struct{}
	maxBatch int
	interval time.Duration
}

// NewBatchWriter 创建批量写入器
func NewBatchWriter(store *ProxyStore, bufSize, maxBatch int, interval time.Duration) *BatchWriter {
	bw := &BatchWriter{
		store:    store,
		ch:       make(chan CheckItem, bufSize),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		maxBatch: maxBatch,
		interval: interval,
	}
	go bw.loop()
	return bw
}

// Submit 提交一条验证结果（非阻塞）
func (bw *BatchWriter) Submit(item CheckItem) {
	select {
	case bw.ch <- item:
	default:
		// channel 满则直接同步写
		bw.store.UpdateCheck(item.Addr, item.Ok, item.LatencyMs, item.Alpha, item.MaxLatMs, item.BlacklistThreshold)
	}
}

// Flush 强制刷新所有待写入数据
func (bw *BatchWriter) Flush() {
	var batch []CheckItem
	for {
		select {
		case item := <-bw.ch:
			batch = append(batch, item)
		default:
			if len(batch) > 0 {
				bw.writeBatch(batch)
			}
			return
		}
	}
}

// Stop 停止批量写入器
func (bw *BatchWriter) Stop() {
	close(bw.stopCh)
	<-bw.doneCh
}

func (bw *BatchWriter) loop() {
	defer close(bw.doneCh)
	ticker := time.NewTicker(bw.interval)
	defer ticker.Stop()

	var batch []CheckItem
	for {
		select {
		case item := <-bw.ch:
			batch = append(batch, item)
			if len(batch) >= bw.maxBatch {
				bw.writeBatch(batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				bw.writeBatch(batch)
				batch = nil
			}
		case <-bw.stopCh:
			// 退出前清空
			close(bw.ch)
			for item := range bw.ch {
				batch = append(batch, item)
			}
			if len(batch) > 0 {
				bw.writeBatch(batch)
			}
			return
		}
	}
}

func (bw *BatchWriter) writeBatch(batch []CheckItem) {
	s := bw.store
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		// fallback: 逐条写
		for _, item := range batch {
			s.updateCheckInner(item)
		}
		return
	}

	for _, item := range batch {
		s.updateCheckTx(tx, item)
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		// fallback
		for _, item := range batch {
			s.updateCheckInner(item)
		}
	}
}

// updateCheckTx 在事务内执行单条 UpdateCheck（不加锁）
func (s *ProxyStore) updateCheckTx(tx *sql.Tx, item CheckItem) {
	var avgLat, sr float64
	var cc, fc int
	err := tx.QueryRow(`SELECT avg_latency, success_rate, check_count, fail_count FROM proxies WHERE addr = ?`, item.Addr).
		Scan(&avgLat, &sr, &cc, &fc)
	if err != nil {
		return
	}

	cc++
	now := time.Now()

	if item.Ok {
		lat := float64(item.LatencyMs)
		if cc == 1 {
			avgLat = lat
			sr = 1.0
		} else {
			sr = item.Alpha*1.0 + (1-item.Alpha)*sr
			avgLat = item.Alpha*lat + (1-item.Alpha)*avgLat
		}
		fc = 0
		score := computeScore(sr, avgLat, item.MaxLatMs)
		tx.Exec(`UPDATE proxies SET
			latency = ?, avg_latency = ?, success_rate = ?,
			check_count = ?, fail_count = 0, score = ?,
			blacklisted = 0, last_checked = ?, last_success = ?
		WHERE addr = ?`,
			item.LatencyMs, avgLat, sr, cc, score, now, now, item.Addr)
	} else {
		sr = item.Alpha*0.0 + (1-item.Alpha)*sr
		fc++
		score := computeScore(sr, avgLat, item.MaxLatMs)
		blacklisted := 0
		if fc >= item.BlacklistThreshold {
			blacklisted = 1
		}
		tx.Exec(`UPDATE proxies SET
			success_rate = ?, check_count = ?, fail_count = ?,
			score = ?, blacklisted = ?, last_checked = ?
		WHERE addr = ?`,
			sr, cc, fc, score, blacklisted, now, item.Addr)
	}
}

// updateCheckInner 不加锁版本的 UpdateCheck（在已持锁的环境中使用）
func (s *ProxyStore) updateCheckInner(item CheckItem) {
	var avgLat, sr float64
	var cc, fc int
	err := s.db.QueryRow(`SELECT avg_latency, success_rate, check_count, fail_count FROM proxies WHERE addr = ?`, item.Addr).
		Scan(&avgLat, &sr, &cc, &fc)
	if err != nil {
		return
	}

	cc++
	now := time.Now()

	if item.Ok {
		lat := float64(item.LatencyMs)
		if cc == 1 {
			avgLat = lat
			sr = 1.0
		} else {
			sr = item.Alpha*1.0 + (1-item.Alpha)*sr
			avgLat = item.Alpha*lat + (1-item.Alpha)*avgLat
		}
		fc = 0
		score := computeScore(sr, avgLat, item.MaxLatMs)
		s.db.Exec(`UPDATE proxies SET
			latency = ?, avg_latency = ?, success_rate = ?,
			check_count = ?, fail_count = 0, score = ?,
			blacklisted = 0, last_checked = ?, last_success = ?
		WHERE addr = ?`,
			item.LatencyMs, avgLat, sr, cc, score, now, now, item.Addr)
	} else {
		sr = item.Alpha*0.0 + (1-item.Alpha)*sr
		fc++
		score := computeScore(sr, avgLat, item.MaxLatMs)
		blacklisted := 0
		if fc >= item.BlacklistThreshold {
			blacklisted = 1
		}
		s.db.Exec(`UPDATE proxies SET
			success_rate = ?, check_count = ?, fail_count = ?,
			score = ?, blacklisted = ?, last_checked = ?
		WHERE addr = ?`,
			sr, cc, fc, score, blacklisted, now, item.Addr)
	}
}
