package xiaozhi

// 把小智回传的原始 Opus 包封装成 Ogg/Opus 容器，便于直接播放：
//   - 浏览器原生支持 audio/ogg; codecs=opus，页面端可直接 <audio> 播放；
//   - 设备端经 ffmpeg 转 mp3 后交 mpg123 播放（复用现有依赖，主程序仍无 cgo）。
//
// 仅做容器封装（lacing + Ogg 页 CRC），不解码音频，故无需任何原生/第三方依赖。
// 参考 RFC 7845（Ogg Encapsulation for Opus）与 RFC 3533（Ogg 容器）。

import "encoding/binary"

// oggCRCTable 是 Ogg 用的 CRC-32 查表（多项式 0x04c11db7，无反转，无最终异或）。
var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

// oggPage 组装单个 Ogg 页，承载一个完整 Opus 包（不跨页）。
// headerType: 0x02=首页(BOS) 0x04=末页(EOS) 0x00=普通页。
func oggPage(headerType byte, granule uint64, serial, seq uint32, packet []byte) []byte {
	// 段表：把包长按 255 拆成 lacing 值（含末尾 <255 的终止字节）。
	nseg := len(packet)/255 + 1
	seg := make([]byte, nseg)
	for i := 0; i < nseg-1; i++ {
		seg[i] = 255
	}
	seg[nseg-1] = byte(len(packet) % 255)

	page := make([]byte, 27+nseg+len(packet))
	copy(page[0:], "OggS")
	page[4] = 0 // 版本
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:], granule)
	binary.LittleEndian.PutUint32(page[14:], serial)
	binary.LittleEndian.PutUint32(page[18:], seq)
	// page[22:26] 为 CRC，先留 0，算完整页后回填。
	page[26] = byte(nseg)
	copy(page[27:], seg)
	copy(page[27+nseg:], packet)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
	return page
}

// muxOggOpus 把一串原始 Opus 包封装为 Ogg/Opus（单声道、原始采样率 16k）。
// granule 以 48k 计（Opus 内部固定 48k），每 60ms 帧推进 2880 个采样。
// preSkip 是解码输出开头要丢弃的采样数：完整流用编码器默认(3840)，
// 而把连续流切成的「逐句分段」必须用 0，否则每段开头都会被砍掉 ~80ms（吞字）。
// packets 为空时返回 nil。
func muxOggOpus(packets [][]byte, preSkip uint16) []byte {
	if len(packets) == 0 {
		return nil
	}
	const serial uint32 = 0x4f50_4553 // 任意固定流序列号
	var seq uint32
	var out []byte

	// 1) 标识头 OpusHead（BOS 页）。
	head := make([]byte, 19)
	copy(head[0:], "OpusHead")
	head[8] = 1                                       // 版本
	head[9] = 1                                       // 声道数
	binary.LittleEndian.PutUint16(head[10:], preSkip) // pre-skip
	binary.LittleEndian.PutUint32(head[12:], 16000)   // 原始输入采样率（信息性）
	binary.LittleEndian.PutUint16(head[16:], 0)       // 输出增益
	head[18] = 0                                      // 声道映射族
	out = append(out, oggPage(0x02, 0, serial, seq, head)...)
	seq++

	// 2) 注释头 OpusTags。
	vendor := "ElectronStudio"
	tags := append([]byte("OpusTags"), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(tags[8:], uint32(len(vendor)))
	tags = append(tags, vendor...)
	tags = append(tags, 0, 0, 0, 0) // 用户注释条数=0
	out = append(out, oggPage(0x00, 0, serial, seq, tags)...)
	seq++

	// 3) 音频页：每页一个 Opus 包，granule 累加。
	var granule uint64
	for i, pkt := range packets {
		granule += 2880 // 60ms @ 48k
		ht := byte(0x00)
		if i == len(packets)-1 {
			ht = 0x04 // EOS
		}
		out = append(out, oggPage(ht, granule, serial, seq, pkt)...)
		seq++
	}
	return out
}
