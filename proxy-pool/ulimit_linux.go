//go:build linux

package main

import (
	"log/slog"
	"syscall"
)

func setUlimit() {
	const target uint64 = 65535
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		slog.Warn("获取 ulimit 失败", "error", err)
		return
	}
	slog.Info("文件描述符限制", "current", rLimit.Cur, "hard", rLimit.Max, "target", target)
	if rLimit.Cur >= target {
		return
	}
	rLimit.Cur = target
	if rLimit.Max < target {
		rLimit.Max = target
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		slog.Warn("自动提升 ulimit 失败", "error", err,
			"fix_1", "ulimit -n 65535",
			"fix_2", "echo '* soft nofile 65535' >> /etc/security/limits.conf",
			"fix_3", "在 service 文件中添加 LimitNOFILE=65535",
		)
	} else {
		slog.Info("文件描述符限制已提升", "target", target)
	}
}
