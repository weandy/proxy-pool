package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Config 服务配置
type Config struct {
	// 服务监听
	ListenAddr string `json:"listen_addr"` // 默认 ":18080"

	// 代理源
	URLsFile string `json:"urls_file"` // URL 源 JSON 文件路径

	// 验证配置（可热更新）
	Concurrency  int    `json:"concurrency"`   // 并发线程数，默认 200
	TimeoutSec   int    `json:"timeout_sec"`   // 超时秒数，默认 10
	VerifyMethod string `json:"verify_method"` // apple / 204 / httpbin

	// GeoIP
	GeoIPPath string `json:"geoip_path"` // 默认 GeoLite2-Country.mmdb

	// 调度（可热更新）
	RefreshIntervalMin int `json:"refresh_interval_min"` // 默认 60 分钟

	// 蜜罐过滤阈值
	HoneypotThreshold int `json:"honeypot_threshold"` // 默认 50

	// API 鉴权
	APIKey      string `json:"api_key"`      // 对外 API Secret Key
	InternalKey string `json:"internal_key"` // 内部管理 API 密钥

	// 持久化 & 评分（可热更新）
	DBPath                 string  `json:"db_path"`
	MaxLatencyMs           int     `json:"max_latency_ms"`
	ScoreDecayAlpha        float64 `json:"score_decay_alpha"`
	BlacklistFailThreshold int     `json:"blacklist_fail_threshold"`
	BlacklistReviveRounds  int     `json:"blacklist_revive_rounds"`

	// 自动淘汰（可热更新）
	AutoPurgeEnabled bool `json:"auto_purge_enabled"` // 默认 true
	AutoPurgeRounds  int  `json:"auto_purge_rounds"`  // 连续 N 轮仍黑名单则删除，默认 3

	// 源自动评级（可热更新）
	SourceAutoRateEnabled bool    `json:"source_auto_rate_enabled"` // 默认 false
	SourceMinValidRate    float64 `json:"source_min_valid_rate"`    // 有效率低于此値停用，默认 1.0%
	SourceMinFetches      int     `json:"source_min_fetches"`       // 最少拓取次数才评判，默认 3

	// Telegram 告警（可热更新）
	TGBotToken      string `json:"tg_bot_token"`
	TGChatID        string `json:"tg_chat_id"`
	TGEnabled       bool   `json:"tg_enabled"`
	AlertMinProxies int    `json:"alert_min_proxies"` // 存量告警阈值，默认 100
}

// DefaultConfig 默认配置
func DefaultConfig() Config {
	return Config{
		ListenAddr:             "127.0.0.1:18080",
		URLsFile:               "../URLS.JSON",
		Concurrency:            200,
		TimeoutSec:             10,
		VerifyMethod:           "apple",
		GeoIPPath:              "GeoLite2-Country.mmdb",
		RefreshIntervalMin:     60,
		HoneypotThreshold:      50,
		APIKey:                 "",
		InternalKey:            "changeme",
		DBPath:                 "proxies.db",
		MaxLatencyMs:           5000,
		ScoreDecayAlpha:        0.3,
		BlacklistFailThreshold: 5,
		BlacklistReviveRounds:  3,
		AutoPurgeEnabled:       true,
		AutoPurgeRounds:        3,
		SourceAutoRateEnabled:  false,
		SourceMinValidRate:     1.0,
		SourceMinFetches:       3,
		TGEnabled:              false,
		AlertMinProxies:        100,
	}
}

// LoadConfig 从文件加载配置
func LoadConfig(path string) Config {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

// SaveConfig 将配置持久化到文件
func SaveConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// HotConfig 线程安全的可热更新配置容器
type HotConfig struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

func NewHotConfig(path string) *HotConfig {
	cfg := LoadConfig(path)
	return &HotConfig{cfg: cfg, path: path}
}

func (h *HotConfig) Get() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Update 热更新可变字段并持久化
func (h *HotConfig) Update(patch map[string]interface{}) (Config, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 用 JSON 序列化当前配置，合并 patch，再反序列化
	cur, _ := json.Marshal(h.cfg)
	var merged map[string]interface{}
	_ = json.Unmarshal(cur, &merged)

	// 允许热更新的字段白名单
	allowed := map[string]bool{
		"concurrency": true, "timeout_sec": true, "verify_method": true,
		"refresh_interval_min": true, "honeypot_threshold": true,
		"max_latency_ms": true, "score_decay_alpha": true,
		"blacklist_fail_threshold": true, "blacklist_revive_rounds": true,
		"api_key":            true,
		"auto_purge_enabled": true, "auto_purge_rounds": true,
		"source_auto_rate_enabled": true, "source_min_valid_rate": true, "source_min_fetches": true,
		"tg_bot_token": true, "tg_chat_id": true, "tg_enabled": true, "alert_min_proxies": true,
	}

	for k, v := range patch {
		if allowed[k] {
			merged[k] = v
		} else {
			return h.cfg, fmt.Errorf("字段 %q 不允许热更新", k)
		}
	}

	data, _ := json.Marshal(merged)
	var newCfg Config
	if err := json.Unmarshal(data, &newCfg); err != nil {
		return h.cfg, err
	}

	// 保留不可热更新的字段
	newCfg.ListenAddr = h.cfg.ListenAddr
	newCfg.URLsFile = h.cfg.URLsFile
	newCfg.GeoIPPath = h.cfg.GeoIPPath
	newCfg.InternalKey = h.cfg.InternalKey
	newCfg.DBPath = h.cfg.DBPath

	h.cfg = newCfg

	// 持久化
	_ = SaveConfig(h.path, h.cfg)

	return h.cfg, nil
}
