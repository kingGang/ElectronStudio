// Package minimax 是 MiniMax(海螺 AI）开放平台的纯 Go 客户端，
// 覆盖会被接入 ElectronStudio 的多模态能力：文本转语音（T2A）、文生图（Image）。
//
// 受“主程序无 cgo”约束，这里只用 net/http + encoding/json，不依赖任何 SDK。
// 鉴权统一 Authorization: Bearer <api_key>（已实测 api.minimaxi.com 上 T2A/图片均无需 GroupId）。
// 端点拼在 baseURL 之后（baseURL 形如 https://api.minimaxi.com/v1）：
//   - 语音：POST {baseURL}/t2a_v2          返回 data.audio（hex 编码音频，需 hex 解码）
//   - 图片：POST {baseURL}/image_generation 返回 data.image_urls（临时下载 URL 数组）
//
// 注意：MiniMax 即使 HTTP 200 也可能业务失败，必须校验 base_resp.status_code == 0。
package minimax

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 默认模型与音色（可被调用方覆盖）。
const (
	DefaultTTSModel   = "speech-02-hd"      // 语音合成模型
	DefaultImageModel = "image-01"          // 文生图模型
	DefaultMusicModel = "music-1.5"         // 音乐生成模型（已实测可用）
	DefaultVoiceID    = "male-qn-qingse"    // 青涩青年音（系统音色）
	defaultTimeout    = 120 * time.Second   // 含图片/音乐等较慢的同步生成
)

// Client 是一个 MiniMax 开放平台客户端。并发安全（无可变状态）。
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New 创建客户端。baseURL 形如 https://api.minimaxi.com/v1（末尾斜杠会被去掉）。
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// baseResp 是 MiniMax 所有响应都带的业务状态。status_code==0 才算成功。
type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// post 发送 JSON POST 到 {baseURL}{path}，带 Bearer 鉴权，返回响应体字节。
// HTTP 非 200 时返回错误（含响应片段）。业务层 status_code 由各调用方自行校验。
func (c *Client) post(ctx context.Context, path string, reqBody any) ([]byte, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("minimax: 序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("minimax: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("minimax: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax: 服务返回 %d: %s", resp.StatusCode, snippet(data, 256))
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// 文本转语音（T2A v2）
// ---------------------------------------------------------------------------

// SpeakOptions 配置一次语音合成。零值字段使用默认。
type SpeakOptions struct {
	Model      string  // 默认 DefaultTTSModel
	VoiceID    string  // 默认 DefaultVoiceID
	Format     string  // 音频格式：mp3(默认)/pcm/wav...
	SampleRate int     // 采样率，默认 24000
	Speed      float32 // 语速 0.5~2.0，默认 1.0
}

type t2aVoiceSetting struct {
	VoiceID string  `json:"voice_id"`
	Speed   float32 `json:"speed,omitempty"`
}
type t2aAudioSetting struct {
	Format     string `json:"format,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
}
type t2aRequest struct {
	Model        string          `json:"model"`
	Text         string          `json:"text"`
	Stream       bool            `json:"stream"`
	OutputFormat string          `json:"output_format"` // hex：data.audio 为十六进制音频
	VoiceSetting t2aVoiceSetting `json:"voice_setting"`
	AudioSetting t2aAudioSetting `json:"audio_setting"`
}
type t2aResponse struct {
	Data struct {
		Audio string `json:"audio"` // hex 编码音频
	} `json:"data"`
	BaseResp baseResp `json:"base_resp"`
}

// Synthesize 把文本合成为音频字节（默认 mp3）。返回可直接交给播放器（mpg123）播放的字节。
func (c *Client) Synthesize(ctx context.Context, text string, opt SpeakOptions) ([]byte, error) {
	if text == "" {
		return nil, fmt.Errorf("minimax: 文本为空")
	}
	model := orDefault(opt.Model, DefaultTTSModel)
	voice := orDefault(opt.VoiceID, DefaultVoiceID)
	format := orDefault(opt.Format, "mp3")
	sr := opt.SampleRate
	if sr <= 0 {
		sr = 24000
	}
	speed := opt.Speed
	if speed <= 0 {
		speed = 1.0
	}
	reqBody := t2aRequest{
		Model: model, Text: text, Stream: false, OutputFormat: "hex",
		VoiceSetting: t2aVoiceSetting{VoiceID: voice, Speed: speed},
		AudioSetting: t2aAudioSetting{Format: format, SampleRate: sr},
	}
	data, err := c.post(ctx, "/t2a_v2", reqBody)
	if err != nil {
		return nil, err
	}
	var r t2aResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("minimax: 解析语音响应失败: %w", err)
	}
	if r.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax: 语音合成失败 (%d): %s", r.BaseResp.StatusCode, r.BaseResp.StatusMsg)
	}
	if r.Data.Audio == "" {
		return nil, fmt.Errorf("minimax: 语音响应无音频数据")
	}
	audio, err := hex.DecodeString(r.Data.Audio)
	if err != nil {
		return nil, fmt.Errorf("minimax: 音频 hex 解码失败: %w", err)
	}
	return audio, nil
}

// ---------------------------------------------------------------------------
// 文生图（Image Generation）
// ---------------------------------------------------------------------------

// ImageOptions 配置一次文生图。零值字段使用默认。
type ImageOptions struct {
	Model       string // 默认 DefaultImageModel
	AspectRatio string // 默认 1:1（圆屏用方图最合适）
	N           int    // 张数，默认 1
}

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	N              int    `json:"n,omitempty"`
	ResponseFormat string `json:"response_format"` // url：返回临时下载链接数组
	PromptOptimizer bool  `json:"prompt_optimizer"`
}
type imageResponse struct {
	Data struct {
		ImageURLs []string `json:"image_urls"`
	} `json:"data"`
	BaseResp baseResp `json:"base_resp"`
}

// GenerateImageURLs 根据提示词生成图片，返回临时下载 URL 数组（限时有效）。
func (c *Client) GenerateImageURLs(ctx context.Context, prompt string, opt ImageOptions) ([]string, error) {
	if prompt == "" {
		return nil, fmt.Errorf("minimax: 提示词为空")
	}
	model := orDefault(opt.Model, DefaultImageModel)
	ratio := orDefault(opt.AspectRatio, "1:1")
	n := opt.N
	if n <= 0 {
		n = 1
	}
	reqBody := imageRequest{
		Model: model, Prompt: prompt, AspectRatio: ratio, N: n,
		ResponseFormat: "url", PromptOptimizer: true,
	}
	data, err := c.post(ctx, "/image_generation", reqBody)
	if err != nil {
		return nil, err
	}
	var r imageResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("minimax: 解析图片响应失败: %w", err)
	}
	if r.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax: 图片生成失败 (%d): %s", r.BaseResp.StatusCode, r.BaseResp.StatusMsg)
	}
	if len(r.Data.ImageURLs) == 0 {
		return nil, fmt.Errorf("minimax: 图片响应无 URL")
	}
	return r.Data.ImageURLs, nil
}

// GenerateImage 生成一张图片并下载为字节（PNG/JPEG）。便于直接推到屏幕/聊天。
func (c *Client) GenerateImage(ctx context.Context, prompt string, opt ImageOptions) ([]byte, error) {
	urls, err := c.GenerateImageURLs(ctx, prompt, opt)
	if err != nil {
		return nil, err
	}
	return c.download(ctx, urls[0])
}

// ---------------------------------------------------------------------------
// 音乐生成（Music Generation）
// ---------------------------------------------------------------------------

// MusicOptions 配置一次音乐生成。零值字段使用默认。
type MusicOptions struct {
	Model      string // 默认 DefaultMusicModel
	Lyrics     string // 可选歌词（含 [verse]/[chorus] 等标记）；为空则倾向纯音乐
	Format     string // 音频格式，默认 mp3
	SampleRate int    // 默认 44100
}

type musicAudioSetting struct {
	SampleRate int    `json:"sample_rate,omitempty"`
	Bitrate    int    `json:"bitrate,omitempty"`
	Format     string `json:"format,omitempty"`
}
type musicRequest struct {
	Model        string            `json:"model"`
	Prompt       string            `json:"prompt"`
	Lyrics       string            `json:"lyrics,omitempty"`
	OutputFormat string            `json:"output_format"`
	AudioSetting musicAudioSetting `json:"audio_setting,omitempty"`
}
type musicResponse struct {
	Data struct {
		Audio string `json:"audio"` // hex 编码音频
	} `json:"data"`
	BaseResp baseResp `json:"base_resp"`
}

// GenerateMusic 按 prompt（风格/情绪/主题）生成一段音乐，返回音频字节（默认 mp3）。
// lyrics 可选；同步返回（data.audio 为 hex，解码即音频）。
func (c *Client) GenerateMusic(ctx context.Context, prompt string, opt MusicOptions) ([]byte, error) {
	if prompt == "" {
		return nil, fmt.Errorf("minimax: 音乐提示词为空")
	}
	sr := opt.SampleRate
	if sr <= 0 {
		sr = 44100
	}
	reqBody := musicRequest{
		Model:        orDefault(opt.Model, DefaultMusicModel),
		Prompt:       prompt,
		Lyrics:       opt.Lyrics,
		OutputFormat: "hex",
		AudioSetting: musicAudioSetting{SampleRate: sr, Bitrate: 256000, Format: orDefault(opt.Format, "mp3")},
	}
	data, err := c.post(ctx, "/music_generation", reqBody)
	if err != nil {
		return nil, err
	}
	var r musicResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("minimax: 解析音乐响应失败: %w", err)
	}
	if r.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax: 音乐生成失败 (%d): %s", r.BaseResp.StatusCode, r.BaseResp.StatusMsg)
	}
	if r.Data.Audio == "" {
		return nil, fmt.Errorf("minimax: 音乐响应无音频数据")
	}
	audio, err := hex.DecodeString(r.Data.Audio)
	if err != nil {
		return nil, fmt.Errorf("minimax: 音乐 hex 解码失败: %w", err)
	}
	return audio, nil
}

// download 下载一个 URL 的内容（用于取回生成的图片字节）。
func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax: 下载图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax: 下载图片返回 %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func snippet(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
