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
)

// ModelConfig 描述一个大模型条目。
type ModelConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // echo | openai
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

// SpeechConfig 描述语音 sidecar 配置。
type SpeechConfig struct {
	SidecarURL string `json:"sidecar_url,omitempty"`
}

// GestureConfig 描述手势 sidecar 配置。
type GestureConfig struct {
	SidecarURL string `json:"sidecar_url,omitempty"`
}

// MusicConfig 描述音乐配置。
type MusicConfig struct {
	Mpg123 string `json:"mpg123,omitempty"` // mpg123 可执行路径，默认 "mpg123"
}

// CameraConfig 描述摄像头采集配置（经 ffmpeg 抓取 UVC 摄像头）。
type CameraConfig struct {
	Enabled     bool   `json:"enabled,omitempty"`
	FFmpeg      string `json:"ffmpeg,omitempty"`       // ffmpeg 可执行路径，默认 "ffmpeg"
	InputFormat string `json:"input_format,omitempty"` // v4l2(Linux) | dshow(Windows) | avfoundation(macOS)
	Input       string `json:"input,omitempty"`        // 设备规格，如 /dev/video0 或 video=Camera
}

// Config 是应用的完整配置。
type Config struct {
	Addr   string        `json:"addr"`
	Robot  string        `json:"robot,omitempty"` // auto | electronbot | mock
	Speech  SpeechConfig  `json:"speech"`
	Gesture GestureConfig `json:"gesture"`
	Camera  CameraConfig  `json:"camera"`
	Music   MusicConfig   `json:"music"`
	Models []ModelConfig `json:"models"`
	Active string        `json:"active,omitempty"`
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
