package choreography

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// 动作的持久化：用户录制的动作存为 JSON，下次启动加载并注册。
// 内置动作（DefaultActions）每次启动重新注册，文件中的同名动作会覆盖之。

// SaveActions 把当前全部动作原子写入 path。
func (e *Engine) SaveActions(path string) error {
	data, err := json.MarshalIndent(e.All(), "", "  ")
	if err != nil {
		return fmt.Errorf("choreography: 序列化动作失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("choreography: 写入失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("choreography: 替换失败: %w", err)
	}
	return nil
}

// LoadActions 从 path 读取动作并逐个注册；文件不存在时静默返回。
func (e *Engine) LoadActions(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("choreography: 读取失败: %w", err)
	}
	var actions []Action
	if err := json.Unmarshal(data, &actions); err != nil {
		return fmt.Errorf("choreography: 解析失败: %w", err)
	}
	for _, a := range actions {
		e.Register(a)
	}
	return nil
}
