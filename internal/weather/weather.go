// Package weather 提供简单的天气查询（基于免 key 的 wttr.in）。
package weather

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 查询天气。
type Client struct {
	http    *http.Client
	baseURL string // 便于测试注入；默认 https://wttr.in
}

// New 创建一个天气客户端。
func New() *Client {
	return &Client{http: &http.Client{Timeout: 8 * time.Second}, baseURL: "https://wttr.in"}
}

// Get 返回某城市的当前天气一行文本（中文）。city 为空时按出口 IP 定位。
func (c *Client) Get(ctx context.Context, city string) (string, error) {
	// format=3 → "城市: 天气 温度"；lang=zh → 中文天气描述。
	u := fmt.Sprintf("%s/%s?format=3&lang=zh", c.baseURL, url.PathEscape(city))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/8") // wttr.in 对 curl UA 返回纯文本
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("weather: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("weather: 返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
