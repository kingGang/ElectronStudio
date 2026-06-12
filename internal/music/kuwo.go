package music

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"time"
)

// KuwoSearcher 对接酷我音乐的 Web 接口。
//
// 注意：这是非官方接口，依赖 csrf token cookie，且接口形态可能随酷我改版变化——属
// 尽力实现，真机/真网络下若失效需按最新接口微调。解析逻辑有单元测试覆盖。
type KuwoSearcher struct {
	client *http.Client
}

// NewKuwoSearcher 创建一个酷我搜索源。
func NewKuwoSearcher() *KuwoSearcher {
	jar, _ := cookiejar.New(nil)
	return &KuwoSearcher{client: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

// kuwoSearchResp / kuwoURLResp 是酷我响应的最小结构（仅取所需字段）。
type kuwoSearchResp struct {
	Data struct {
		List []struct {
			RID      json.Number `json:"rid"`
			Name     string      `json:"name"`
			Artist   string      `json:"artist"`
			Duration json.Number `json:"duration"`
		} `json:"list"`
	} `json:"data"`
}
type kuwoURLResp struct {
	Data struct {
		URL string `json:"url"`
	} `json:"data"`
}

// token 拉取 csrf token（酷我把它放在 kw_token cookie 里）。
func (k *KuwoSearcher) token(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://www.kuwo.cn/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := k.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	u, _ := url.Parse("http://www.kuwo.cn/")
	for _, c := range k.client.Jar.Cookies(u) {
		if c.Name == "kw_token" {
			return c.Value, nil
		}
	}
	return "", fmt.Errorf("music: 未取到 kuwo token")
}

func (k *KuwoSearcher) get(ctx context.Context, rawURL, token string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "http://www.kuwo.cn/")
	if token != "" {
		req.Header.Set("csrf", token)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("music: kuwo 返回 %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Search 实现 Searcher。
func (k *KuwoSearcher) Search(ctx context.Context, query string) ([]Track, error) {
	token, err := k.token(ctx)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("http://www.kuwo.cn/api/www/search/searchMusicBykeyWord?key=%s&pn=1&rn=10",
		url.QueryEscape(query))
	var r kuwoSearchResp
	if err := k.get(ctx, u, token, &r); err != nil {
		return nil, err
	}
	tracks := make([]Track, 0, len(r.Data.List))
	for _, it := range r.Data.List {
		dur, _ := strconv.Atoi(it.Duration.String())
		tracks = append(tracks, Track{
			ID: it.RID.String(), Name: it.Name, Artist: it.Artist, Duration: dur,
		})
	}
	return tracks, nil
}

// ResolveURL 实现 Searcher：取曲目可播放地址。
func (k *KuwoSearcher) ResolveURL(ctx context.Context, t Track) (string, error) {
	if t.URL != "" {
		return t.URL, nil
	}
	token, _ := k.token(ctx)
	u := fmt.Sprintf("http://www.kuwo.cn/api/v1/www/music/playUrl?mid=%s&type=music&httpsStatus=1", t.ID)
	var r kuwoURLResp
	if err := k.get(ctx, u, token, &r); err != nil {
		return "", err
	}
	if r.Data.URL == "" {
		return "", fmt.Errorf("music: 未取到播放地址")
	}
	return r.Data.URL, nil
}
