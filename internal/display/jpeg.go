package display

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"

	"github.com/kingGang/ElectronStudio/internal/robot"
)

// EncodeJPEG 把一帧 240×240 RGB888 编码为 JPEG（用于发给视觉模型）。
func EncodeJPEG(rgb []byte) ([]byte, error) {
	if len(rgb) != robot.ImageBytesRGB888 {
		return nil, fmt.Errorf("display: 帧尺寸不符, 期望 %d 实际 %d", robot.ImageBytesRGB888, len(rgb))
	}
	img := image.NewRGBA(image.Rect(0, 0, scrW, scrH))
	for i, p := 0, 0; i < len(rgb); i, p = i+3, p+4 {
		img.Pix[p] = rgb[i]
		img.Pix[p+1] = rgb[i+1]
		img.Pix[p+2] = rgb[i+2]
		img.Pix[p+3] = 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
