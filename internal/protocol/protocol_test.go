package protocol

import (
	"testing"
)

// TestEncodeDecodeRoundTrip 验证「编码 → 解码 → As」的完整往返：
// 编码后的信封类型应取自负载，解码后能无损还原负载内容。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := SendTextCommand{Text: "你好小电"}

	data, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}

	env, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if env.Type != TypeSendText {
		t.Fatalf("信封类型错误: 期望 %q 实际 %q", TypeSendText, env.Type)
	}
	if env.TS == 0 {
		t.Error("时间戳未自动填充")
	}

	out, err := As[SendTextCommand](env)
	if err != nil {
		t.Fatalf("As 失败: %v", err)
	}
	if out.Text != in.Text {
		t.Fatalf("内容不一致: 期望 %q 实际 %q", in.Text, out.Text)
	}
}

// TestAsTypeMismatch 验证 As 能在类型与负载不匹配时报错，而非返回脏数据。
func TestAsTypeMismatch(t *testing.T) {
	data, err := Encode(VADEvent{Speaking: true, Level: 0.5})
	if err != nil {
		t.Fatalf("Encode 失败: %v", err)
	}
	env, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	// 用错误的目标类型解析, 应当报错。
	if _, err := As[SendTextCommand](env); err == nil {
		t.Fatal("类型不匹配时 As 应返回错误, 但返回了 nil")
	}
}

// TestDecodeEmptyType 验证缺少 type 字段的信封会被拒绝。
func TestDecodeEmptyType(t *testing.T) {
	if _, err := Decode([]byte(`{"ts":1}`)); err == nil {
		t.Fatal("缺少 type 字段时 Decode 应报错")
	}
}

// TestEncodeSeq 验证带序号编码能正确写入并回读 seq。
func TestEncodeSeq(t *testing.T) {
	data, err := EncodeSeq(InterruptCommand{Reason: "user"}, 42)
	if err != nil {
		t.Fatalf("EncodeSeq 失败: %v", err)
	}
	env, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if env.Seq != 42 {
		t.Fatalf("序号错误: 期望 42 实际 %d", env.Seq)
	}
}

// TestFrameRoundTrip 验证镜像帧的二进制打包/解包往返一致。
func TestFrameRoundTrip(t *testing.T) {
	const w, h = 4, 2
	hdr := FrameHeader{Width: w, Height: h, Format: PixelRGB888, Seq: 7}
	pixels := make([]byte, w*h*3) // 4*2*3 = 24 字节
	for i := range pixels {
		pixels[i] = byte(i)
	}

	packed, err := EncodeFrame(hdr, pixels)
	if err != nil {
		t.Fatalf("EncodeFrame 失败: %v", err)
	}
	if len(packed) != FrameHeaderSize+len(pixels) {
		t.Fatalf("打包长度错误: %d", len(packed))
	}

	gotHdr, gotPixels, err := DecodeFrame(packed)
	if err != nil {
		t.Fatalf("DecodeFrame 失败: %v", err)
	}
	if gotHdr != hdr {
		t.Fatalf("帧头不一致: 期望 %+v 实际 %+v", hdr, gotHdr)
	}
	for i := range pixels {
		if gotPixels[i] != pixels[i] {
			t.Fatalf("像素[%d]不一致: 期望 %d 实际 %d", i, pixels[i], gotPixels[i])
		}
	}
}

// TestEncodeFrameLengthMismatch 验证像素长度与帧头不符时拒绝打包。
func TestEncodeFrameLengthMismatch(t *testing.T) {
	hdr := FrameHeader{Width: 4, Height: 2, Format: PixelRGB888}
	if _, err := EncodeFrame(hdr, []byte{1, 2, 3}); err == nil {
		t.Fatal("像素长度不符时 EncodeFrame 应报错")
	}
}

// TestDecodeFrameBadMagic 验证非法魔数会被拒绝。
func TestDecodeFrameBadMagic(t *testing.T) {
	bad := make([]byte, FrameHeaderSize)
	if _, _, err := DecodeFrame(bad); err == nil {
		t.Fatal("魔数错误时 DecodeFrame 应报错")
	}
}
