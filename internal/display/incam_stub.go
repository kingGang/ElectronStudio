//go:build !darwin || !cgo

package display

import "fmt"

// 非 (darwin && cgo) 构建:不支持进程内采集。CameraSource 会自动回落到 camcap/ffmpeg 子进程,
// 保持 CGO_ENABLED=0 的纯 Go 交叉编译(树莓派/Windows/无 cgo 的 macOS)。

func incamSupported() bool { return false }

func incamSetOrientation(rotate int, mirror bool) {}

func incamStart(cs *CameraSource, nameSub string, side int) error {
	return fmt.Errorf("display: 进程内采集仅 darwin+cgo 支持")
}

func incamStop() {}

