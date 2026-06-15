package music

// QQ 音乐扫码登录（纯 Go 实现腾讯 ptlogin 二维码授权链）：
//
//	ptqrshow 出二维码 → 轮询 ptqrlogin → 成功后 check_sig 拿 p_skey
//	→ graph.qq.com/oauth2.0/authorize 拿 code → musicu music.login.LoginServer/Login
//	→ 得到 musickey(=qm_keyst) 与 musicid(uin)，拼成登录 cookie。
//
// 非官方流程，依赖腾讯接口形态，可能随其改版失效。前几步（出码/轮询）已在真实网络验证。
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	qqLoginAppID  = "716027609" // QZone appid（出码用）
	qqMusicThirdA = "100497308" // QQ 音乐第三方 appid
)

// QRLogin 持有一次扫码登录会话（含 cookie jar）。
type QRLogin struct {
	follow   *http.Client // 跟随重定向
	noFollow *http.Client // 不跟随（取 Location 里的 code）
	qrsig    string
}

// QRResult 是一次轮询的结果。
type QRResult struct {
	State   string // waiting(未扫) | scanned(已扫待确认) | expired(已失效) | ok(成功) | error
	Message string
	Cookie  string // State==ok 时为可用的登录 cookie
}

var ptuiCBRe = regexp.MustCompile(`ptuiCB\('(\d+)','(\d+)','(.*?)','(\d+)','(.*?)',\s*'(.*?)'\)`)

// NewQRLogin 创建一次扫码登录会话。
func NewQRLogin() *QRLogin {
	jar, _ := cookiejar.New(nil)
	return &QRLogin{
		follow:   &http.Client{Jar: jar, Timeout: 15 * time.Second},
		noFollow: &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// Start 拉取二维码 PNG，并记录本次会话的 qrsig。
func (q *QRLogin) Start(ctx context.Context) ([]byte, error) {
	u := fmt.Sprintf("https://ssl.ptlogin2.qq.com/ptqrshow?appid=%s&e=2&l=M&s=3&d=72&v=4&t=%s&daid=383&pt_3rd_aid=%s",
		qqLoginAppID, randT(), qqMusicThirdA)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://xui.ptlogin2.qq.com/")
	resp, err := q.follow.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "qrsig" {
			q.qrsig = c.Value
		}
	}
	if q.qrsig == "" {
		return nil, fmt.Errorf("qqlogin: 未取到 qrsig")
	}
	return io.ReadAll(resp.Body)
}

// Poll 轮询一次扫码状态；扫码确认后自动走完授权并返回登录 cookie。
func (q *QRLogin) Poll(ctx context.Context) (QRResult, error) {
	if q.qrsig == "" {
		return QRResult{State: "error", Message: "尚未开始"}, fmt.Errorf("qqlogin: 无 qrsig")
	}
	u := fmt.Sprintf("https://ssl.ptlogin2.qq.com/ptqrlogin?u1=%s&ptqrtoken=%d&ptredirect=0&h=1&t=1&g=1&from_ui=1&ptlang=2052&action=0-0-%d&js_ver=20102616&js_type=1&login_sig=&pt_uistyle=40&aid=%s&daid=383&pt_3rd_aid=%s&",
		url.QueryEscape("https://graph.qq.com/oauth2.0/login_jump"), hash33(q.qrsig), time.Now().UnixMilli(), qqLoginAppID, qqMusicThirdA)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://xui.ptlogin2.qq.com/")
	resp, err := q.follow.Do(req)
	if err != nil {
		return QRResult{State: "error", Message: err.Error()}, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := ptuiCBRe.FindStringSubmatch(string(body))
	if m == nil {
		return QRResult{State: "error", Message: "无法解析登录返回"}, fmt.Errorf("qqlogin: ptuiCB 解析失败: %s", body)
	}
	switch m[1] {
	case "66":
		return QRResult{State: "waiting", Message: "等待扫码"}, nil
	case "67":
		return QRResult{State: "scanned", Message: "已扫码，请在手机上确认"}, nil
	case "65":
		return QRResult{State: "expired", Message: "二维码已失效，请刷新"}, nil
	case "0":
		cookie, err := q.finish(ctx, m[3])
		if err != nil {
			return QRResult{State: "error", Message: err.Error()}, err
		}
		return QRResult{State: "ok", Message: "登录成功：" + m[6], Cookie: cookie}, nil
	default:
		return QRResult{State: "error", Message: m[5]}, fmt.Errorf("qqlogin: 未知状态 %s: %s", m[1], m[5])
	}
}

// finish 走完授权链：check_sig → authorize 取 code → musicu Login 取 musickey。
func (q *QRLogin) finish(ctx context.Context, checkSigURL string) (string, error) {
	// 1) check_sig（跟随重定向），收集 p_skey / uin 等 cookie 到 jar。
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, checkSigURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if resp, err := q.follow.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} else {
		return "", fmt.Errorf("check_sig 失败: %w", err)
	}
	// 2) 取 graph.qq.com 下的 p_skey，算 g_tk。
	gURL, _ := url.Parse("https://graph.qq.com")
	var pskey string
	for _, c := range q.follow.Jar.Cookies(gURL) {
		if c.Name == "p_skey" {
			pskey = c.Value
		}
	}
	if pskey == "" {
		return "", fmt.Errorf("未取到 p_skey（授权失败）")
	}
	// 3) authorize 拿 code（不跟随重定向，从 Location 抓 code）。
	form := url.Values{
		"response_type": {"code"},
		"client_id":     {qqMusicThirdA},
		"redirect_uri":  {"https://y.qq.com/portal/wx_redirect.html?login_type=1&surl=https://y.qq.com/"},
		"scope":         {"get_user_info,get_app_friends"},
		"state":         {"state"},
		"switch":        {""},
		"from_ptlogin":  {"1"},
		"src":           {"1"},
		"update_auth":   {"1"},
		"openapi":       {"1010_1030"},
		"g_tk":          {strconv.Itoa(gtk(pskey))},
		"auth_time":     {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		"ui":            {uuid()},
	}
	areq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://graph.qq.com/oauth2.0/authorize", strings.NewReader(form.Encode()))
	areq.Header.Set("User-Agent", "Mozilla/5.0")
	areq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	areq.Header.Set("Referer", "https://graph.qq.com/")
	aresp, err := q.noFollow.Do(areq)
	if err != nil {
		return "", fmt.Errorf("authorize 失败: %w", err)
	}
	loc := aresp.Header.Get("Location")
	io.Copy(io.Discard, aresp.Body)
	aresp.Body.Close()
	cm := regexp.MustCompile(`code=([^&]+)`).FindStringSubmatch(loc)
	if cm == nil {
		return "", fmt.Errorf("未取到授权 code（Location=%q）", loc)
	}
	code := cm[1]
	// 4) musicu Login 用 code 换 musickey。
	return q.musicLogin(ctx, code)
}

type qqMusicLoginResp struct {
	Req1 struct {
		Code int `json:"code"`
		Data struct {
			MusicID  json.Number `json:"musicid"`
			MusicKey string      `json:"musickey"`
			EUIN     string      `json:"euin"`
		} `json:"data"`
	} `json:"req1"`
}

func (q *QRLogin) musicLogin(ctx context.Context, code string) (string, error) {
	reqBody := map[string]any{
		"comm": map[string]any{"tmeAppID": "qqmusic", "tmeLoginType": 2},
		"req1": map[string]any{
			"module": "music.login.LoginServer",
			"method": "Login",
			"param":  map[string]any{"code": code},
		},
	}
	payload, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://u.y.qq.com/cgi-bin/musicu.fcg", bytes.NewReader(payload))
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://y.qq.com/")
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.follow.Do(req)
	if err != nil {
		return "", fmt.Errorf("musicu Login 失败: %w", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var r qqMusicLoginResp
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("musicu Login 解析失败: %w (%s)", err, snippet(data))
	}
	if r.Req1.Code != 0 || r.Req1.Data.MusicKey == "" {
		return "", fmt.Errorf("musicu Login 返回异常 code=%d (%s)", r.Req1.Code, snippet(data))
	}
	uin := strings.TrimLeft(r.Req1.Data.MusicID.String(), "o0")
	key := r.Req1.Data.MusicKey
	return fmt.Sprintf("uin=%s; tmeLoginType=2; euin=%s; qm_keyst=%s; qqmusic_key=%s",
		uin, r.Req1.Data.EUIN, key, key), nil
}

// hash33 计算 ptqrtoken（按 32 位有符号语义复刻腾讯 JS 实现）。
func hash33(s string) int {
	var t int32
	for _, c := range s {
		t += (t << 5) + int32(c)
		t &= 0x7fffffff
	}
	return int(t)
}

// gtk 由 p_skey 计算 g_tk（QQ 系通用算法，基数 5381）。
func gtk(s string) int {
	var h int32 = 5381
	for _, c := range s {
		h += (h << 5) + int32(c)
		h &= 0x7fffffff
	}
	return int(h)
}

func randT() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	v := float64(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3])) / 4294967296.0
	return strconv.FormatFloat(v, 'f', 16, 64)
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
