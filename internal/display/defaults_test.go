package display

import "testing"

// TestDefaultEmotionsDecode 确保每个内置默认表情都能被加载器解码（帧数/尺寸在上限内），
// 否则它在运行时会被静默跳过、回退程序脸。
func TestDefaultEmotionsDecode(t *testing.T) {
	defs := defaultEmotionGIFs()
	if len(defs) == 0 {
		t.Fatal("未嵌入任何默认表情")
	}
	for name, data := range defs {
		clip, err := decodeGIFClip(data)
		if err != nil {
			t.Errorf("默认表情 %q 解码失败: %v", name, err)
			continue
		}
		if len(clip.Frames) == 0 {
			t.Errorf("默认表情 %q 无帧", name)
		}
	}
	t.Logf("已校验 %d 个内置默认表情", len(defs))
}

// TestSeedDefaultEmotions 校验播种逻辑：首次写入、marker 记录后不重播、尊重已存在素材。
func TestSeedDefaultEmotions(t *testing.T) {
	dir := t.TempDir()

	// 首次：应写入全部默认表情。
	n1, err := SeedDefaultEmotions(dir)
	if err != nil {
		t.Fatalf("首次播种失败: %v", err)
	}
	if n1 == 0 {
		t.Fatal("首次播种应写入若干默认表情，实际 0")
	}

	// 再次：marker 已记录，应一个都不再写。
	n2, err := SeedDefaultEmotions(dir)
	if err != nil {
		t.Fatalf("二次播种失败: %v", err)
	}
	if n2 != 0 {
		t.Errorf("二次播种应为 0（marker 拦截），实际 %d", n2)
	}
}
