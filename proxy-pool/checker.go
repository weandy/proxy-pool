package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/geoip2-golang"
)

// ==================== 全局抓取工具 ====================

var proxyLineRegex = regexp.MustCompile(`(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):(\d{1,5})`)

var fetchHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// safeFetchClient 用于抓取代理源 URL（启用 TLS 证书验证，防止中间人攻击）
var safeFetchClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
	},
}

// privateIPBlocks 私网 IP 段
var privateIPBlocks []*net.IPNet

func init() {
	cidrs := []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"127.0.0.0/8", "169.254.0.0/16", "0.0.0.0/8",
		"224.0.0.0/4", "240.0.0.0/4", "100.64.0.0/10",
		"192.0.0.0/24", "192.0.2.0/24", "198.51.100.0/24",
		"203.0.113.0/24",
	}
	for _, cidr := range cidrs {
		_, block, _ := net.ParseCIDR(cidr)
		privateIPBlocks = append(privateIPBlocks, block)
	}
}

func isPrivateIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// ==================== GeoIP ====================

var (
	geoipDB      *geoip2.Reader
	geoipEnabled bool
)

func initGeoIP(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err2 := downloadGeoIPDB(path); err2 != nil {
			return err2
		}
	}
	db, err := geoip2.Open(path)
	if err != nil {
		return err
	}
	geoipDB = db
	geoipEnabled = true
	return nil
}

func closeGeoIP() {
	if geoipDB != nil {
		geoipDB.Close()
	}
}

func downloadGeoIPDB(path string) error {
	urls := []string{
		"https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-Country.mmdb",
		"https://git.io/GeoLite2-Country.mmdb",
	}
	for _, u := range urls {
		resp, err := fetchHTTPClient.Get(u)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		tmp := path + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			resp.Body.Close()
			continue
		}
		written, err := io.Copy(f, resp.Body)
		resp.Body.Close()
		f.Close()
		if err != nil || written < 1024*1024 {
			os.Remove(tmp)
			continue
		}
		return os.Rename(tmp, path)
	}
	return fmt.Errorf("GeoIP 数据库所有下载源均失败")
}

func getCountryCode(ipStr string) string {
	if !geoipEnabled || geoipDB == nil {
		return "XX"
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "XX"
	}
	record, err := geoipDB.Country(ip)
	if err != nil || record.Country.IsoCode == "" {
		return "XX"
	}
	return record.Country.IsoCode
}

// ==================== 下载代理列表
// loadURLsFromFile 从本地文件加载 URL
func loadURLsFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// 兜底尝试当前目录的 URLS.JSON
		data, err = os.ReadFile("URLS.JSON")
		if err != nil {
			return nil, err
		}
	}
	var urls []string
	if json.Unmarshal(data, &urls) == nil {
		return urls, nil
	}
	// 按行解析
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") &&
			(strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) {
			urls = append(urls, line)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("未找到有效 URL")
	}
	return urls, nil
}

func fetchContent(rawURL string) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := safeFetchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func extractProxies(content string) []string {
	matches := proxyLineRegex.FindAllStringSubmatch(content, -1)
	seen := make(map[string]struct{})
	var result []string
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		port, err := strconv.Atoi(m[2])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		ip := net.ParseIP(m[1])
		if ip == nil || isPrivateIP(ip) {
			continue
		}
		key := fmt.Sprintf("%s:%d", m[1], port)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			result = append(result, key)
		}
	}
	return result
}

// filterHoneypots 蜜罐IP过滤（同一IP开放端口数超过阈值则过滤）
func filterHoneypots(proxies []string, threshold int) []string {
	portCount := make(map[string]int)
	for _, p := range proxies {
		host, _, _ := net.SplitHostPort(p)
		portCount[host]++
	}
	honeypots := make(map[string]bool)
	for ip, cnt := range portCount {
		if cnt > threshold {
			honeypots[ip] = true
		}
	}
	var result []string
	for _, p := range proxies {
		host, _, _ := net.SplitHostPort(p)
		if !honeypots[host] {
			result = append(result, p)
		}
	}
	return result
}

// SourceFetchResult 单个源的抓取结果
type SourceFetchResult struct {
	URL      string
	RawCount int    // 原始抓取数（未去重）
	NewCount int    // 新增数（去重后）
	FetchOK  bool   // 是否成功
	FetchErr string // 错误信息
}

// fetchAllProxies 并发抓取所有 URL 源并返回去重后的代理列表 + 来源映射 + 每源统计
func fetchAllProxies(urls []string) ([]string, map[string]string, []SourceFetchResult) {
	var mu sync.Mutex
	seen := make(map[string]struct{})
	sourceMap := make(map[string]string) // proxy addr → source URL
	var all []string
	var fetchResults []SourceFetchResult
	sem := make(chan struct{}, 50)
	var wg sync.WaitGroup

	for _, u := range urls {
		wg.Add(1)
		go func(rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := SourceFetchResult{URL: rawURL, FetchOK: true}

			content, err := fetchContent(rawURL)
			if err != nil {
				result.FetchOK = false
				result.FetchErr = err.Error()
				mu.Lock()
				fetchResults = append(fetchResults, result)
				mu.Unlock()
				return
			}
			proxies := extractProxies(content)
			result.RawCount = len(proxies)

			mu.Lock()
			for _, p := range proxies {
				if _, ok := seen[p]; !ok {
					seen[p] = struct{}{}
					all = append(all, p)
					sourceMap[p] = rawURL
					result.NewCount++
				}
			}
			fetchResults = append(fetchResults, result)
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	return all, sourceMap, fetchResults
}

// ==================== 代理验证 ====================

func createTransport(proxyURL *url.URL, timeoutSec int) *http.Transport {
	d := time.Duration(timeoutSec) * time.Second
	return &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   d,
			KeepAlive: 0,
		}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:     true,
		DisableCompression:    true,
		ResponseHeaderTimeout: d,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
	}
}

func verifyTarget(method string) string {
	switch method {
	case "204":
		return "http://connectivitycheck.gstatic.com/generate_204"
	case "httpbin":
		return "http://httpbin.org/ip"
	default: // apple
		return "http://www.apple.com/library/test/success.html"
	}
}

// verifyTargetHTTPS 返回 HTTPS 验证目标（GeoIP 分流）
func verifyTargetHTTPS(method string, country string) string {
	switch method {
	case "204":
		if country == "CN" {
			return "https://connectivitycheck.platform.hicloud.com/generate_204"
		}
		return "https://connectivitycheck.gstatic.com/generate_204"
	case "httpbin":
		return "https://httpbin.org/ip"
	default: // apple
		return "https://www.apple.com/library/test/success.html"
	}
}

func isBodyValid(method, body string) bool {
	switch method {
	case "204":
		return len(strings.TrimSpace(body)) == 0
	case "httpbin":
		return strings.Contains(body, "origin")
	default: // apple
		return strings.Contains(body, "<TITLE>Success</TITLE>") &&
			strings.Contains(body, "<BODY>Success</BODY>") && len(body) < 500
	}
}

// CheckResult 单次验证结果（用于进度上报）
type CheckResult struct {
	Addr      string
	Ok        bool
	LatencyMs int64
	Done      int64
	Total     int64
	Alive     int64
}

// RunVerifyPipeline 验证代理，通过 progressCb 上报每条结果（不再直接写 store）
func RunVerifyPipeline(
	ctx context.Context,
	cfg Config,
	proxies []string,
	progressCb func(CheckResult),
) {
	target := verifyTarget(cfg.VerifyMethod)
	total := int64(len(proxies))
	var done, alive atomic.Int64

	jobs := make(chan string, cfg.Concurrency*2)
	var wg sync.WaitGroup

	workers := cfg.Concurrency
	if len(proxies) < workers {
		workers = len(proxies)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case p, ok := <-jobs:
					if !ok {
						return
					}
					ok2, latMs := checkOne(ctx, p, target, cfg.TimeoutSec, cfg.VerifyMethod)
					done.Add(1)
					if ok2 {
						alive.Add(1)
					}
					if progressCb != nil {
						progressCb(CheckResult{
							Addr:      p,
							Ok:        ok2,
							LatencyMs: latMs,
							Done:      done.Load(),
							Total:     total,
							Alive:     alive.Load(),
						})
					}
				}
			}
		}()
	}

	go func() {
		for _, p := range proxies {
			select {
			case <-ctx.Done():
				break
			case jobs <- p:
			}
		}
		close(jobs)
	}()

	wg.Wait()
}

// RunVerifyHTTPS 第二轮 HTTPS 复测（只验证 HTTP 阶段通过的代理）
func RunVerifyHTTPS(
	ctx context.Context,
	cfg Config,
	httpPassed []string, // 第一轮通过的代理
	countryMap map[string]string, // addr → country code
	progressCb func(CheckResult),
) {
	total := int64(len(httpPassed))
	var done, alive atomic.Int64

	jobs := make(chan string, cfg.Concurrency*2)
	var wg sync.WaitGroup

	workers := cfg.Concurrency
	if len(httpPassed) < workers {
		workers = len(httpPassed)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case p, ok := <-jobs:
					if !ok {
						return
					}
					country := countryMap[p]
					target := verifyTargetHTTPS(cfg.VerifyMethod, country)
					ok2, latMs := checkOne(ctx, p, target, cfg.TimeoutSec, cfg.VerifyMethod)
					done.Add(1)
					if ok2 {
						alive.Add(1)
					}
					if progressCb != nil {
						progressCb(CheckResult{
							Addr:      p,
							Ok:        ok2,
							LatencyMs: latMs,
							Done:      done.Load(),
							Total:     total,
							Alive:     alive.Load(),
						})
					}
				}
			}
		}()
	}

	go func() {
		for _, p := range httpPassed {
			select {
			case <-ctx.Done():
				break
			case jobs <- p:
			}
		}
		close(jobs)
	}()

	wg.Wait()
}

func checkOne(ctx context.Context, proxyAddr, target string, timeoutSec int, method string) (bool, int64) {
	pURL, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		return false, 0
	}
	transport := createTransport(pURL, timeoutSec)
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(timeoutSec*2) * time.Second,
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", target, nil)
	if err != nil {
		return false, 0
	}
	req.Close = true
	req.Header.Set("User-Agent", "Mozilla/5.0")

	start := time.Now()
	resp, err := client.Do(req)
	latMs := time.Since(start).Milliseconds()
	if err != nil {
		return false, latMs
	}
	defer resp.Body.Close()

	lr := io.LimitReader(resp.Body, 2048)
	body, _ := io.ReadAll(lr)

	if method == "204" {
		return resp.StatusCode == 204 && isBodyValid(method, string(body)), latMs
	}
	return resp.StatusCode == 200 && isBodyValid(method, string(body)), latMs
}
