package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ipAPIResult ip-api.com 批量查询的单条返回
type ipAPIResult struct {
	Status  string `json:"status"`
	Query   string `json:"query"`
	Country string `json:"country"`
	ISP     string `json:"isp"`
	Org     string `json:"org"`
	AS      string `json:"as"`
	Hosting bool   `json:"hosting"`
	Proxy   bool   `json:"proxy"`
}

// DetectIPTypes 批量检测未标记代理的 IP 类型（IDC/ISP）
// 使用 ip-api.com 的免费批量接口：POST http://ip-api.com/batch
// 限制：每批最多 100 个，每分钟最多 15 批（45 req/min 留余量）
func DetectIPTypes(store *ProxyStore) int {
	const batchSize = 100
	const interval = 4500 * time.Millisecond // ~13 批/分钟，留余量

	total := 0
	client := &http.Client{Timeout: 15 * time.Second}

	for {
		addrs := store.ListUndetectedIPs(batchSize)
		if len(addrs) == 0 {
			break
		}

		// 构建查询体：从 addr 提取纯 IP
		type queryItem struct {
			Query  string `json:"query"`
			Fields string `json:"fields"`
		}
		var batch []queryItem
		addrMap := make(map[string]string) // ip -> addr

		for _, addr := range addrs {
			ip := addr
			if host, _, err := net.SplitHostPort(addr); err == nil {
				ip = host
			}
			batch = append(batch, queryItem{
				Query:  ip,
				Fields: "status,query,country,isp,org,as,hosting,proxy",
			})
			addrMap[ip] = addr
		}

		body, _ := json.Marshal(batch)
		resp, err := client.Post("http://ip-api.com/batch", "application/json", bytes.NewReader(body))
		if err != nil {
			slog.Warn("IP检测 批量请求失败", "error", err)
			time.Sleep(interval)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var results []ipAPIResult
		if err := json.Unmarshal(respBody, &results); err != nil {
			slog.Warn("IP检测 解析响应失败", "error", err)
			time.Sleep(interval)
			continue
		}

		for _, r := range results {
			if r.Status != "success" {
				continue
			}
			addr, ok := addrMap[r.Query]
			if !ok {
				// 尝试从 addrs 直接匹配
				for _, a := range addrs {
					if strings.HasPrefix(a, r.Query+":") || a == r.Query {
						addr = a
						ok = true
						break
					}
				}
			}
			if !ok {
				continue
			}

			ipType := "isp"
			if r.Hosting {
				ipType = "idc"
			}

			risk := 0
			if r.Proxy {
				risk = 50
			}
			if r.Hosting {
				risk += 20
			}

			store.UpdateIPInfo(addr, ipType, r.ISP, r.AS, risk)
			total++
		}

		slog.Info("IP检测 本批完成", "batch", len(results), "total", total)

		// 限速
		time.Sleep(interval)
	}

	if total > 0 {
		slog.Info("IP检测全部完成", "total", total)
	}
	return total
}
