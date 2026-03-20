//go:build !linux

package main

func setUlimit() {
	// 非 Linux 平台无需设置 ulimit
}
