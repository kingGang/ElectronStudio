package config

import (
	"path/filepath"
	"testing"
)

// TestLoadMissingReturnsDefault 验证文件不存在时返回默认配置。
func TestLoadMissingReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if c.Active != "echo" || len(c.Models) != 1 {
		t.Fatalf("默认配置错误: %+v", c)
	}
}

// TestSaveLoadRoundTrip 验证保存后重新加载内容一致。
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Default()
	c.Upsert(ModelConfig{Name: "GPT", Type: "openai", BaseURL: "https://api", APIKey: "k", Model: "gpt-4o"})
	c.SetActive("openai:gpt-4o")
	if err := c.Save(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got.Active != "openai:gpt-4o" || len(got.Models) != 2 {
		t.Fatalf("往返不一致: %+v", got)
	}
}

// TestRemoveActiveReselects 验证删除当前生效模型后自动改选。
func TestRemoveActiveReselects(t *testing.T) {
	c := Default()
	id := c.Upsert(ModelConfig{Name: "GPT", Type: "openai", Model: "gpt-4o"})
	c.SetActive(id)
	if !c.Remove(id) {
		t.Fatal("删除应成功")
	}
	if c.Active != "echo" {
		t.Fatalf("删除生效模型后应改选, 实际 %q", c.Active)
	}
}

// TestUpsertReplaces 验证同 ID 覆盖而非重复添加。
func TestUpsertReplaces(t *testing.T) {
	c := Default()
	c.Upsert(ModelConfig{ID: "x", Name: "A", Type: "echo"})
	c.Upsert(ModelConfig{ID: "x", Name: "B", Type: "echo"})
	count := 0
	for _, m := range c.Models {
		if m.ID == "x" {
			count++
			if m.Name != "B" {
				t.Fatalf("未覆盖: %q", m.Name)
			}
		}
	}
	if count != 1 {
		t.Fatalf("应只有一条 x, 实际 %d", count)
	}
}
