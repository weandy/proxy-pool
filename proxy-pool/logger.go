package main

import (
	"log/slog"
	"os"
)

// initLogger 初始化结构化日志（slog），支持 TEXT 和 JSON 两种输出格式
func initLogger(jsonMode bool) {
	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	if jsonMode {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}
