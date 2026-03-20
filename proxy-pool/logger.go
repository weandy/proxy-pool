package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ==================== 环形缓冲区日志 ====================

// LogEntry 单条日志
type LogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"msg"`
}

// RingLog 环形缓冲区，保存最近 N 条日志
type RingLog struct {
	mu      sync.RWMutex
	entries []LogEntry
	cap     int
	pos     int
	full    bool
}

func NewRingLog(capacity int) *RingLog {
	return &RingLog{
		entries: make([]LogEntry, capacity),
		cap:     capacity,
	}
}

// Append 添加一条日志
func (r *RingLog) Append(entry LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.pos] = entry
	r.pos++
	if r.pos >= r.cap {
		r.pos = 0
		r.full = true
	}
}

// Recent 获取最近 N 条日志（按时间正序）
func (r *RingLog) Recent(n int) []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := r.pos
	if r.full {
		total = r.cap
	}
	if n <= 0 || n > total {
		n = total
	}

	result := make([]LogEntry, 0, n)
	if r.full {
		// 从最旧的位置开始，取最后 n 条
		start := r.pos - n
		if start < 0 {
			start += r.cap
		}
		for i := 0; i < n; i++ {
			idx := (start + i) % r.cap
			result = append(result, r.entries[idx])
		}
	} else {
		start := r.pos - n
		if start < 0 {
			start = 0
		}
		for i := start; i < r.pos; i++ {
			result = append(result, r.entries[i])
		}
	}
	return result
}

// ==================== 全局实例 ====================

var globalRingLog = NewRingLog(1000)

// GetRecentLogs 外部读取日志
func GetRecentLogs(n int) []LogEntry {
	return globalRingLog.Recent(n)
}

// ==================== 日志初始化 ====================

// initLogger 初始化日志：同时写入 stdout + 日志文件 + 环形缓冲区
func initLogger(jsonMode bool) {
	// 打开日志文件（追加模式）
	logFile, err := os.OpenFile("engine.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法打开日志文件: %v, 仅输出到 stdout\n", err)
		logFile = nil
	}

	// 多路写入器：stdout + 文件
	var writers []io.Writer
	writers = append(writers, os.Stdout)
	if logFile != nil {
		writers = append(writers, logFile)
	}
	multiWriter := io.MultiWriter(writers...)

	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}

	var baseHandler slog.Handler
	if jsonMode {
		baseHandler = slog.NewJSONHandler(multiWriter, opts)
	} else {
		baseHandler = slog.NewTextHandler(multiWriter, opts)
	}

	// 包装 handler：在写入 base handler 的同时，写入环形缓冲区
	handler := &ringHandler{base: baseHandler, ring: globalRingLog}
	slog.SetDefault(slog.New(handler))
}

// ringHandler 包装 slog.Handler，额外写入环形缓冲区
type ringHandler struct {
	base slog.Handler
	ring *RingLog
}

func (h *ringHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *ringHandler) Handle(ctx context.Context, r slog.Record) error {
	// 1. 写入 base handler（stdout + 文件）
	_ = h.base.Handle(ctx, r)

	// 2. 写入环形缓冲区
	// 收集属性
	msg := r.Message
	attrs := ""
	r.Attrs(func(a slog.Attr) bool {
		attrs += " " + a.Key + "=" + a.Value.String()
		return true
	})

	h.ring.Append(LogEntry{
		Time:    r.Time.Format(time.DateTime),
		Level:   r.Level.String(),
		Message: msg + attrs,
	})
	return nil
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ringHandler{base: h.base.WithAttrs(attrs), ring: h.ring}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{base: h.base.WithGroup(name), ring: h.ring}
}
