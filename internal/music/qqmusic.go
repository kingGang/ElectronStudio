package music

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 防风控参数：请求限速 + 取流地址缓存 + 失败退避。目标是让调用频率/形态贴近正常听歌，
// 避免短时间高频取流触发腾讯风控（详见 README 风险说明）。
const (
	qqMinInterval = 800 * time.Millisecond // 两次请求最小间隔
	qqURLTTL      = 20 * time.Minute       // 取流地址缓存时长（vkey 有效期内复用，少打接口）
	qqFailBackoff = 5 * time.Second        // 命中风控/失败后的冷却
)

// QQMusicSearcher 对接 QQ 音乐的 Web 接口。
//
// 注意：非官方接口，且"取可播放地址"需向 vkey 服务换取 purl——匿名状态通常只能拿到
// 免费曲/试听片段；要放完整付费曲需带登录后的 cookie（uin + qqmusic_key）。接口形态可能
// 随 QQ 音乐改版变化，失效时按最新接口微调；解析逻辑有单元测试覆盖。
type QQMusicSearcher struct {
	client *http.Client
	cookie string // 整串 cookie（最稳）；为空则用 uin/key 拼
	uin    string // 登录 QQ 号；匿名留空（按 "0" 处理）
	key    string // qm_keyst / qqmusic_key 值

	mu      sync.Mutex
	lastReq time.Time             // 上次请求时间（用于限速/退避）
	cache   map[string]cachedURL // trackID -> 已解析的播放地址（带过期）
}

type cachedURL struct {
	url string
	exp time.Time
}

// throttle 串行化请求并保证最小间隔；返回前已等待到允许发起的时刻。
func (q *QQMusicSearcher) throttle() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if d := qqMinInterval - time.Since(q.lastReq); d > 0 {
		time.Sleep(d)
	}
	q.lastReq = time.Now()
}

// backoff 命中风控/失败后拉长下次允许请求的时间。
func (q *QQMusicSearcher) backoff() {
	q.mu.Lock()
	q.lastReq = time.Now().Add(qqFailBackoff - qqMinInterval)
	q.mu.Unlock()
}

// NewQQMusicSearcher 创建 QQ 音乐源。三者都可留空（匿名，仅免费/试听）。
// cookie 优先：传整串 cookie 时直接原样发送，并从中解析出 uin 供取流参数使用。
func NewQQMusicSearcher(cookie, uin, key string) *QQMusicSearcher {
	if uin == "" && cookie != "" {
		uin = uinFromCookie(cookie)
	}
	return &QQMusicSearcher{
		client: &http.Client{Timeout: 10 * time.Second},
		cookie: cookie,
		uin:    uin,
		key:    key,
	}
}

// uinFromCookie 从整串 cookie 里取 uin 的数字部分（去掉常见的 o0 前缀）。
func uinFromCookie(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == "uin" {
			return strings.TrimLeft(kv[1], "o0")
		}
	}
	return ""
}

// cookieHeader 构造请求用 Cookie 头：优先整串；否则用 uin/key 拼（key 同时挂在
// qm_keyst 与 qqmusic_key 两个名下以兼容不同登录方式）。
func (q *QQMusicSearcher) cookieHeader() string {
	if q.cookie != "" {
		return q.cookie
	}
	if q.uin != "" && q.key != "" {
		return fmt.Sprintf("uin=%s; qm_keyst=%s; qqmusic_key=%s", q.uin, q.key, q.key)
	}
	return ""
}

// qqGUID 是换取 vkey 所需的设备标识。注意：实测必须用 "10000"，换其它值（如随机十位数）
// 取流会返回 104009 登录态校验失败；"10000" 才能配合登录 cookie 拿到 purl。
const qqGUID = "10000"

// qqSearchResp 对应新版 musicu.fcg 搜索模块响应：req_1.data.body.song.list[]。
// （旧的 c.y.qq.com/client_search_cp 已废弃，匿名直接 500。）
type qqSearchResp struct {
	Req1 struct {
		Data struct {
			Body struct {
				Song struct {
					List []struct {
						Mid    string `json:"mid"`
						Name   string `json:"name"`
						Singer []struct {
							Name string `json:"name"`
						} `json:"singer"`
						Interval int `json:"interval"`
					} `json:"list"`
				} `json:"song"`
			} `json:"body"`
		} `json:"data"`
	} `json:"req_1"`
}

type qqVkeyResp struct {
	Req0 struct {
		Code int `json:"code"` // 0=正常；非0多为风控/登录态问题
		Data struct {
			Sip        []string `json:"sip"`
			MidURLInfo []struct {
				PURL string `json:"purl"`
			} `json:"midurlinfo"`
		} `json:"data"`
	} `json:"req_0"`
}

func (q *QQMusicSearcher) do(ctx context.Context, rawURL string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://y.qq.com/")
	if ck := q.cookieHeader(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("music: qq 返回 %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Search 实现 Searcher（走新版 musicu.fcg 搜索模块，匿名可用）。
func (q *QQMusicSearcher) Search(ctx context.Context, query string) ([]Track, error) {
	reqData := map[string]any{
		"req_1": map[string]any{
			"module": "music.search.SearchCgiService",
			"method": "DoSearchForQQMusicDesktop",
			"param": map[string]any{
				"query":        query,
				"num_per_page": 10,
				"page_num":     1,
			},
		},
	}
	payload, _ := json.Marshal(reqData)
	u := "https://u.y.qq.com/cgi-bin/musicu.fcg?format=json&data=" + url.QueryEscape(string(payload))
	q.throttle()
	var r qqSearchResp
	if err := q.do(ctx, u, &r); err != nil {
		return nil, err
	}
	list := r.Req1.Data.Body.Song.List
	tracks := make([]Track, 0, len(list))
	for _, it := range list {
		names := make([]string, 0, len(it.Singer))
		for _, s := range it.Singer {
			names = append(names, s.Name)
		}
		tracks = append(tracks, Track{
			ID: it.Mid, Name: it.Name, Artist: strings.Join(names, "/"), Duration: it.Interval,
		})
	}
	return tracks, nil
}

// ResolveURL 实现 Searcher：向 vkey 服务换取可播放地址（purl 为空表示无版权/需会员）。
// 带缓存（同曲短时间复用，少打接口）、限速与失败退避，降低触发风控的概率。
func (q *QQMusicSearcher) ResolveURL(ctx context.Context, t Track) (string, error) {
	if t.URL != "" {
		return t.URL, nil
	}
	// 缓存命中：直接复用（vkey 有效期内）。
	q.mu.Lock()
	if c, ok := q.cache[t.ID]; ok && time.Now().Before(c.exp) {
		q.mu.Unlock()
		return c.url, nil
	}
	q.mu.Unlock()
	uin := q.uin
	if uin == "" {
		uin = "0"
	}
	// 载荷形态实测要点：req_0 + 顶层 loginUin + guid=10000，配合登录 cookie 才不会 104009。
	reqData := map[string]any{
		"req_0": map[string]any{
			"module": "vkey.GetVkeyServer",
			"method": "CgiGetVkey",
			"param": map[string]any{
				"guid":      qqGUID,
				"songmid":   []string{t.ID},
				"songtype":  []int{0},
				"uin":       uin,
				"loginflag": 1,
				"platform":  "20",
			},
		},
		"loginUin": uin,
		"comm":     map[string]any{"uin": uin, "format": "json", "ct": 24, "cv": 0},
	}
	payload, _ := json.Marshal(reqData)
	u := "https://u.y.qq.com/cgi-bin/musicu.fcg?format=json&data=" + url.QueryEscape(string(payload))
	q.throttle()
	var r qqVkeyResp
	if err := q.do(ctx, u, &r); err != nil {
		q.backoff()
		return "", err
	}
	if r.Req0.Code != 0 {
		q.backoff() // 命中风控/登录态异常：拉长冷却，避免硬重试火上浇油
		return "", fmt.Errorf("music: qq 取流被拒 code=%d（风控或登录态失效，已冷却）", r.Req0.Code)
	}
	if len(r.Req0.Data.MidURLInfo) == 0 || r.Req0.Data.MidURLInfo[0].PURL == "" {
		return "", fmt.Errorf("music: qq 未取到播放地址（可能需会员、无版权或登录态失效）")
	}
	host := "https://ws.stream.qqmusic.qq.com/"
	if len(r.Req0.Data.Sip) > 0 && r.Req0.Data.Sip[0] != "" {
		host = r.Req0.Data.Sip[0]
	}
	full := strings.TrimRight(host, "/") + "/" + strings.TrimLeft(r.Req0.Data.MidURLInfo[0].PURL, "/")
	q.mu.Lock()
	if q.cache == nil {
		q.cache = make(map[string]cachedURL)
	}
	q.cache[t.ID] = cachedURL{url: full, exp: time.Now().Add(qqURLTTL)}
	q.mu.Unlock()
	return full, nil
}

// 确保实现了 Searcher（编译期检查）。
var _ Searcher = (*QQMusicSearcher)(nil)
