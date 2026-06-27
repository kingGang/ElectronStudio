// Package config 负责应用配置的加载与持久化（JSON 文件）。
//
// 配置涵盖监听地址、语音 sidecar 地址、以及可由用户在“设置页”增删的大模型列表。
// 设计为可被设置页回写：修改后调用 Save 落盘，下次启动即生效。
//
// 注意：模型的 API Key 以明文存储于本地配置文件，适用于自托管/单机场景。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ModelConfig 描述一个大模型条目。
type ModelConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // echo | openai | xiaozhi
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
	// 小智(xiaozhi)对接字段（type=xiaozhi 时用）：
	WSURL    string `json:"ws_url,omitempty"`    // 小智 WebSocket 地址，如 wss://api.tenclass.net/xiaozhi/v1/
	OTAURL   string `json:"ota_url,omitempty"`   // 可选；填了则先走 OTA 激活换取 ws 地址与 token
	Token    string `json:"token,omitempty"`     // 直连用的访问令牌（自建服务器可留空）
	DeviceID string `json:"device_id,omitempty"` // 设备 MAC；留空自动生成
	ClientID string `json:"client_id,omitempty"` // 客户端 UUID；留空自动生成
}

// SpeechConfig 描述语音配置：本地 sidecar + 可选的网络 TTS/ASR（OpenAI 兼容）。
type SpeechConfig struct {
	SidecarURL string        `json:"sidecar_url,omitempty"`
	TTS        NetSpeechTTS  `json:"tts,omitempty"` // 网络 TTS（io.tts_engine=openai 时用）
	ASR        NetSpeechASR  `json:"asr,omitempty"` // 网络 ASR（io.audio_in=network 时用）
}

// NetSpeechTTS 是 OpenAI 兼容的文字转语音服务配置（POST {base_url}/audio/speech）。
type NetSpeechTTS struct {
	BaseURL string `json:"base_url,omitempty"` // 形如 https://api.openai.com/v1
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`  // 如 tts-1 / gpt-4o-mini-tts
	Voice   string `json:"voice,omitempty"`  // 如 alloy
	Format  string `json:"format,omitempty"` // 默认 mp3
}

// NetSpeechASR 是 OpenAI 兼容的语音识别服务配置（POST {base_url}/audio/transcriptions）。
type NetSpeechASR struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"` // 如 whisper-1
}

// GestureConfig 描述手势 sidecar 配置。
type GestureConfig struct {
	SidecarURL string `json:"sidecar_url,omitempty"`
}

// MusicConfig 描述音乐配置。
type MusicConfig struct {
	Mpg123 string `json:"mpg123,omitempty"` // mpg123 可执行路径，默认 "mpg123"
	Source string `json:"source,omitempty"` // 音源：qq | kuwo，默认 kuwo
	QQ     QQConfig `json:"qq,omitempty"`   // QQ 音乐凭据（source=qq 时用）
}

// QQConfig 是 QQ 音乐的登录凭据。匿名也能搜与放免费/试听曲；
// 要放完整付费曲需带登录后的 cookie。
//
// 推荐填 Cookie：登录 y.qq.com 后在控制台执行 document.cookie 取整串，原样粘进来——
// QQ 校验登录态需要 uin/qm_keyst/tmeLoginType 等多个 cookie，只填两三个值往往 104009。
// 单独的 UIN/Key 仅在不方便取整串时作降级用。
type QQConfig struct {
	Cookie string `json:"cookie,omitempty"`      // 整串 cookie（document.cookie），最稳
	UIN    string `json:"uin,omitempty"`         // 登录 QQ 号（数字）；Cookie 已含则可留空
	Key    string `json:"qqmusic_key,omitempty"` // qm_keyst / qqmusic_key 值（降级用）
}

// SourceOr 返回音源，默认 kuwo。
func (m MusicConfig) SourceOr() string {
	if m.Source == "qq" {
		return "qq"
	}
	return "kuwo"
}

// CameraConfig 描述摄像头采集配置（经 ffmpeg 抓取 UVC 摄像头）。
type CameraConfig struct {
	Enabled     bool   `json:"enabled,omitempty"`
	FFmpeg      string `json:"ffmpeg,omitempty"`       // ffmpeg 可执行路径，默认 "ffmpeg"
	InputFormat string `json:"input_format,omitempty"` // v4l2(Linux) | dshow(Windows) | avfoundation(macOS)
	Input       string `json:"input,omitempty"`        // 设备规格，如 /dev/video0 或 video=Camera
}

// MiniMaxConfig 是 MiniMax 多模态（文生图 / 语音合成）的凭据与默认参数。
// BaseURL/APIKey 留空时，由 Config.ResolveMiniMax 复用 Models 里的 minimax 大模型条目。
type MiniMaxConfig struct {
	BaseURL    string `json:"base_url,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	GroupID    string `json:"group_id,omitempty"`    // files/upload 与 voice_clone 端点用；留空时由 JWT 自动解析
	VoiceID    string `json:"voice_id,omitempty"`    // T2A 音色，默认 male-qn-qingse
	TTSModel   string `json:"tts_model,omitempty"`   // 语音模型，默认 speech-02-hd
	ImageModel string `json:"image_model,omitempty"` // 文生图模型，默认 image-01
	MusicModel string `json:"music_model,omitempty"` // 音乐模型，默认 music-1.5
}

// IOConfig 配置输入/输出的去向（设备 vs 页面）。设备=ElectronBot 外设(主)，页面=调试镜像。
type IOConfig struct {
	AudioIn   string        `json:"audio_in,omitempty"`   // device | page | off（默认 device：设备麦经 sidecar）
	AudioOut  string        `json:"audio_out,omitempty"`  // device | page | both | off（默认 device：mpg123 播到设备扬声器）
	// AudioDevice：设备侧音频输出的目标声卡名（子串）。仅 macOS 生效：填了就改用 playto helper
	// 把 TTS 定向到该 USB 声卡（如 ElectronBot 语音板的 "USB audio CODEC"），不动系统默认输出；
	// 留空则用 mpg123 播到系统默认输出。
	AudioDevice string        `json:"audio_device,omitempty"`
	TTSEngine   string        `json:"tts_engine,omitempty"` // minimax | sidecar（默认 minimax：云端出 mp3 直接播）
	ImageOut    string        `json:"image_out,omitempty"`  // device | page | both（默认 both：设备屏+页面镜像）
	// ServoEnable：舵机总开关。默认 false（不下发使能）——舵机 I²C 没接通/失联时，主控固件
	// 会因对舵机的 I²C 无限重试而卡死整机；关着此开关即不碰舵机，屏幕/网页/语音照常用。
	// 确认舵机在电子脑壳里能正常控制（即 I²C 已通）后，再设为 true 启用真机舵机。
	ServoEnable bool          `json:"servo_enable,omitempty"`
	MiniMax     MiniMaxConfig `json:"minimax"`
}

// 各路由的默认值（空配置时）。
func (io IOConfig) AudioInOr() string   { return orStr(io.AudioIn, "device") }
func (io IOConfig) AudioOutOr() string  { return orStr(io.AudioOut, "device") }
func (io IOConfig) TTSEngineOr() string { return orStr(io.TTSEngine, "minimax") }
func (io IOConfig) ImageOutOr() string  { return orStr(io.ImageOut, "both") }

// Config 是应用的完整配置。
type Config struct {
	Addr    string        `json:"addr"`
	Robot   string        `json:"robot,omitempty"`   // auto | electronbot | mock
	Persona       string  `json:"persona,omitempty"`        // 设备角色/人设（作为系统提示的人设部分）
	PersonaSource string  `json:"persona_source,omitempty"` // local(用本机角色) | model(用模型自带角色,如小智服务端设定)，默认 local
	Voice         string  `json:"voice,omitempty"`          // 声音音色（覆盖当前 TTS 引擎的音色）
	Speech  SpeechConfig  `json:"speech"`
	Gesture GestureConfig `json:"gesture"`
	Camera  CameraConfig  `json:"camera"`
	Music   MusicConfig   `json:"music"`
	IO      IOConfig      `json:"io"`
	Models  []ModelConfig `json:"models"`
	Active  string        `json:"active,omitempty"`
}

// ResolveMiniMax 返回最终生效的 MiniMax 凭据：IO.MiniMax 优先；BaseURL/APIKey 缺失时
// 复用 Models 里 base_url 含 "minimax" 的大模型条目（用户通常已在那里填好了 key）。
func (c *Config) ResolveMiniMax() MiniMaxConfig {
	m := c.IO.MiniMax
	if m.BaseURL == "" || m.APIKey == "" {
		for _, mc := range c.Models {
			if strings.Contains(strings.ToLower(mc.BaseURL), "minimax") {
				if m.BaseURL == "" {
					m.BaseURL = mc.BaseURL
				}
				if m.APIKey == "" {
					m.APIKey = mc.APIKey
				}
				break
			}
		}
	}
	return m
}

// PersonaSourceOr 返回角色来源，默认 local（用本机角色）。
func (c *Config) PersonaSourceOr() string {
	if c.PersonaSource == "model" {
		return "model"
	}
	return "local"
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Default 返回内置默认配置（仅含本地 Echo 模型，保证开箱即跑）。
// Robot 默认 auto：插上真机即自动连接，否则回退 Mock。
func Default() *Config {
	return &Config{
		Addr:   ":8080",
		Robot:  "auto",
		Models: []ModelConfig{{ID: "echo", Name: "本地回声", Type: "echo"}},
		Active: "echo",
	}
}

// Load 从 path 读取配置；文件不存在时返回默认配置（不报错）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: 读取失败: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config: 解析失败: %w", err)
	}
	c.normalize()
	return &c, nil
}

// Save 原子写入配置到 path（先写临时文件再重命名，避免中途损坏）。
func (c *Config) Save(path string) error {
	c.normalize()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: 序列化失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("config: 写入失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: 替换失败: %w", err)
	}
	return nil
}

// Upsert 新增或按 ID 覆盖一个模型。返回最终的 ID（为空时自动生成）。
func (c *Config) Upsert(m ModelConfig) string {
	if m.ID == "" {
		m.ID = genID(m)
	}
	for i := range c.Models {
		if c.Models[i].ID == m.ID {
			c.Models[i] = m
			return m.ID
		}
	}
	c.Models = append(c.Models, m)
	return m.ID
}

// Remove 按 ID 删除一个模型；删除当前生效模型时会自动改选其一。
func (c *Config) Remove(id string) bool {
	for i := range c.Models {
		if c.Models[i].ID == id {
			c.Models = append(c.Models[:i], c.Models[i+1:]...)
			c.normalize()
			return true
		}
	}
	return false
}

// SetActive 设置当前生效模型；ID 不存在则返回 false。
func (c *Config) SetActive(id string) bool {
	for _, m := range c.Models {
		if m.ID == id {
			c.Active = id
			return true
		}
	}
	return false
}

// normalize 保证至少有一个模型且 Active 指向存在的模型。
func (c *Config) normalize() {
	if len(c.Models) == 0 {
		c.Models = Default().Models
	}
	for _, m := range c.Models {
		if m.ID == c.Active {
			return
		}
	}
	c.Active = c.Models[0].ID
}

// genID 为缺省 ID 的模型生成一个稳定标识。
func genID(m ModelConfig) string {
	base := m.Type
	if m.Model != "" {
		base += ":" + m.Model
	} else if m.Name != "" {
		base += ":" + m.Name
	}
	return base
}
