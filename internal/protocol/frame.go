package protocol

// 本文件定义机器人屏幕镜像帧的二进制传输格式。
//
// 屏幕镜像是 240×240 像素、高频（约 30fps）的数据流，若塞进 JSON 信封并做
// base64 编码会带来约 33% 的体积膨胀与额外的编解码开销。因此镜像帧改走
// WebSocket【二进制帧】通道：每帧 = 固定长度的二进制头 + 原始像素数据。
//
// 前端通过 WebSocket 消息是文本还是二进制即可区分两类通道：
// 文本 → Envelope（JSON 事件/命令），二进制 → 这里定义的镜像帧。

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PixelFormat 表示镜像帧的像素格式。
type PixelFormat uint8

const (
	PixelRGB888 PixelFormat = 1 // 每像素 3 字节 R,G,B
	PixelRGB565 PixelFormat = 2 // 每像素 2 字节, 大端
)

// BytesPerPixel 返回该像素格式每个像素占用的字节数。
func (f PixelFormat) BytesPerPixel() int {
	switch f {
	case PixelRGB888:
		return 3
	case PixelRGB565:
		return 2
	default:
		return 0
	}
}

// frameMagic 是镜像帧头的魔数，用于快速校验与版本识别（"EBF" + 版本号 1）。
var frameMagic = [4]byte{'E', 'B', 'F', '1'}

// FrameHeaderSize 是二进制帧头的固定字节数。
//
// 布局（小端，除像素数据外）：
//
//	偏移  大小  字段
//	0     4     magic   "EBF1"
//	4     2     width   宽（像素）
//	6     2     height  高（像素）
//	8     1     format  PixelFormat
//	9     1     flags   保留标志位（当前为 0）
//	10    4     seq     帧序号
//	---- 共 14 字节 ----
const FrameHeaderSize = 14

// FrameHeader 描述一帧镜像数据的元信息。
type FrameHeader struct {
	Width  uint16
	Height uint16
	Format PixelFormat
	Flags  uint8
	Seq    uint32
}

// PayloadSize 返回该帧头所描述的像素数据应有的字节数。
func (h FrameHeader) PayloadSize() int {
	return int(h.Width) * int(h.Height) * h.Format.BytesPerPixel()
}

// EncodeFrame 将帧头与像素数据打包为一个可直接作为 WebSocket 二进制帧发送的切片。
// 它会校验 pixels 的长度与帧头描述一致，避免发出残缺帧。
func EncodeFrame(h FrameHeader, pixels []byte) ([]byte, error) {
	if h.Format.BytesPerPixel() == 0 {
		return nil, fmt.Errorf("protocol: 未知像素格式 %d", h.Format)
	}
	if want := h.PayloadSize(); want != len(pixels) {
		return nil, fmt.Errorf("protocol: 像素数据长度不符, 期望 %d 实际 %d", want, len(pixels))
	}
	buf := make([]byte, FrameHeaderSize+len(pixels))
	copy(buf[0:4], frameMagic[:])
	binary.LittleEndian.PutUint16(buf[4:6], h.Width)
	binary.LittleEndian.PutUint16(buf[6:8], h.Height)
	buf[8] = byte(h.Format)
	buf[9] = h.Flags
	binary.LittleEndian.PutUint32(buf[10:14], h.Seq)
	copy(buf[FrameHeaderSize:], pixels)
	return buf, nil
}

// ErrShortFrame 表示数据长度不足以容纳一个完整的帧头。
var ErrShortFrame = errors.New("protocol: 数据长度不足一个帧头")

// ErrBadMagic 表示帧头魔数不匹配，数据不是合法的镜像帧。
var ErrBadMagic = errors.New("protocol: 镜像帧魔数不匹配")

// DecodeFrame 从一个 WebSocket 二进制帧解析出帧头与像素数据。
// 返回的像素切片是底层数组的引用（零拷贝），调用方若需长期持有应自行复制。
func DecodeFrame(data []byte) (FrameHeader, []byte, error) {
	if len(data) < FrameHeaderSize {
		return FrameHeader{}, nil, ErrShortFrame
	}
	if [4]byte(data[0:4]) != frameMagic {
		return FrameHeader{}, nil, ErrBadMagic
	}
	h := FrameHeader{
		Width:  binary.LittleEndian.Uint16(data[4:6]),
		Height: binary.LittleEndian.Uint16(data[6:8]),
		Format: PixelFormat(data[8]),
		Flags:  data[9],
		Seq:    binary.LittleEndian.Uint32(data[10:14]),
	}
	pixels := data[FrameHeaderSize:]
	if want := h.PayloadSize(); want != len(pixels) {
		return h, nil, fmt.Errorf("protocol: 像素数据长度不符, 期望 %d 实际 %d", want, len(pixels))
	}
	return h, pixels, nil
}
