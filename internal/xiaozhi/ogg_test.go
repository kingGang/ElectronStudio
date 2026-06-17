package xiaozhi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestMuxOggOpusStructure 校验封装出的 Ogg/Opus 容器结构与每页 CRC 是否自洽。
func TestMuxOggOpusStructure(t *testing.T) {
	// 三个假 Opus 包（内容随意，封装层不解码）。
	packets := [][]byte{
		bytes.Repeat([]byte{0x01}, 40),
		bytes.Repeat([]byte{0x02}, 300), // 跨多个 lacing 段
		{0x03},
	}
	out := muxOggOpus(packets, 3840)
	if len(out) == 0 {
		t.Fatal("muxOggOpus 返回空")
	}

	var pages [][]byte // 逐页切分
	rest := out
	for len(rest) > 0 {
		if string(rest[0:4]) != "OggS" {
			t.Fatalf("页头不是 OggS：% x", rest[0:4])
		}
		nseg := int(rest[26])
		hdr := 27 + nseg
		body := 0
		for _, s := range rest[27 : 27+nseg] {
			body += int(s)
		}
		end := hdr + body
		pages = append(pages, rest[:end])
		rest = rest[end:]
	}

	// OpusHead + OpusTags + 3 音频页 = 5 页。
	if len(pages) != 5 {
		t.Fatalf("页数=%d，期望 5", len(pages))
	}

	// 每页 CRC 自洽：把 CRC 字段清零重算应等于存储值。
	for i, pg := range pages {
		stored := binary.LittleEndian.Uint32(pg[22:26])
		zeroed := append([]byte(nil), pg...)
		binary.LittleEndian.PutUint32(zeroed[22:26], 0)
		if got := oggCRC(zeroed); got != stored {
			t.Errorf("第 %d 页 CRC 不符：got=%08x stored=%08x", i, got, stored)
		}
	}

	// 首页 BOS 标志 + OpusHead 包。
	if pages[0][5] != 0x02 {
		t.Errorf("首页应置 BOS(0x02)，实际 %#x", pages[0][5])
	}
	if string(pages[0][28:36]) != "OpusHead" {
		t.Errorf("首页包不是 OpusHead：%q", pages[0][28:36])
	}
	// 末页 EOS 标志。
	if last := pages[len(pages)-1]; last[5] != 0x04 {
		t.Errorf("末页应置 EOS(0x04)，实际 %#x", last[5])
	}
}

func TestMuxOggOpusEmpty(t *testing.T) {
	if muxOggOpus(nil, 0) != nil {
		t.Error("空输入应返回 nil")
	}
}
