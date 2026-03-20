package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// SendTelegram 发送 Telegram 消息
func SendTelegram(token, chatID, message string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "HTML",
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("TG 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("TG 返回 %d", resp.StatusCode)
	}
	return nil
}

// NotifyRoundSummary 每轮验证结束推送摘要
func NotifyRoundSummary(cfg Config, store *ProxyStore, purged int) {
	if !cfg.TGEnabled || cfg.TGBotToken == "" || cfg.TGChatID == "" {
		return
	}

	snap := GetSnapshot()
	msg := fmt.Sprintf(
		"<b>代理池验证完成</b>\n"+
			"存活: <b>%d</b> | HTTPS: %d\n"+
			"IDC: %d | ISP: %d\n"+
			"今日新增: %d | 本轮淘汰: %d\n"+
			"均分: %.1f | 均延迟: %.0fms\n"+
			"黑名单: %d",
		snap.TotalProxies, snap.HTTPSCount,
		snap.IDCCount, snap.ISPCount,
		snap.TodayNew, purged,
		snap.AvgScore, snap.AvgLatency,
		snap.BlacklistedCount,
	)

	if err := SendTelegram(cfg.TGBotToken, cfg.TGChatID, msg); err != nil {
		slog.Error("TG 推送失败", "error", err)
	} else {
		slog.Info("TG 验证摘要已推送")
	}
}

// CheckLowStockAlert 存量低于阈值时告警
func CheckLowStockAlert(cfg Config, total int) {
	if !cfg.TGEnabled || cfg.TGBotToken == "" || cfg.TGChatID == "" {
		return
	}
	if cfg.AlertMinProxies > 0 && total < cfg.AlertMinProxies {
		msg := fmt.Sprintf(
			"<b>⚠ 代理池存量告警</b>\n当前存活: <b>%d</b>（低于阈值 %d）",
			total, cfg.AlertMinProxies,
		)
		if err := SendTelegram(cfg.TGBotToken, cfg.TGChatID, msg); err != nil {
			slog.Error("TG 告警推送失败", "error", err)
		}
	}
}
