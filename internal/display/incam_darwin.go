//go:build darwin && cgo

package display

// 进程内 macOS 摄像头采集(cgo + AVFoundation)——等价官方 ElectronBot.DotNet 的 MediaCapture
// 进程内拿帧。每帧由 AVFoundation 直接回调进 Go(goCamFrame)，【没有 camcap 子进程，也没有管道读】
// ——彻底消除"app 高频读摄像头管道帧"这条与 libusb(IOKit) 争抢、卡死设备屏的路径。
//
// Objective-C 实现放在同目录 incam_darwin.m(C 定义不能放 cgo 注释里，否则多目标重复符号)。
// 仅在 `CGO_ENABLED=1` 的 macOS 构建里编译;其他构建走 incam_stub.go，自动回落 camcap 子进程。

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework AVFoundation -framework CoreMedia -framework CoreVideo
#include <stdlib.h>
#include <stdint.h>

int  esCamAuth(void);                         // 同步申请摄像头权限，返回 1=已授权
void esCamSetOrientation(int rotate, int mirror); // 送屏画面的旋转(0/90/180/270)与镜像
int  esCamStart(const char* nameSub, int side); // 启动流式采集，0=成功
void esCamStop(void);                          // 停止流式采集
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// 进程内采集每帧通过 goCamFrame 回调到这里;单摄像头，用包级目标即可(无需 cgo.Handle)。
var (
	inprocMu     sync.Mutex
	inprocTarget *CameraSource
)

// incamSupported 报告本构建是否支持进程内采集(darwin+cgo)。
func incamSupported() bool { return true }

// incamSetOrientation 设置送屏画面的旋转/镜像（须在 incamStart 之前调用）。
func incamSetOrientation(rotate int, mirror bool) {
	m := 0
	if mirror {
		m = 1
	}
	C.esCamSetOrientation(C.int(rotate), C.int(m))
}

// incamStart 进程内启动 AVFoundation 采集;每帧回调 goCamFrame → cs.pushInprocFrame。
func incamStart(cs *CameraSource, nameSub string, side int) error {
	if C.esCamAuth() != 1 {
		return fmt.Errorf("display: 无摄像头权限(请在 系统设置→隐私与安全性→摄像头 里允许本程序)")
	}
	inprocMu.Lock()
	inprocTarget = cs
	inprocMu.Unlock()
	cn := C.CString(nameSub)
	defer C.free(unsafe.Pointer(cn))
	if rc := C.esCamStart(cn, C.int(side)); rc != 0 {
		inprocMu.Lock()
		inprocTarget = nil
		inprocMu.Unlock()
		return fmt.Errorf("display: 进程内采集启动失败 rc=%d", int(rc))
	}
	return nil
}

// incamStop 停止进程内采集。
func incamStop() {
	C.esCamStop()
	inprocMu.Lock()
	inprocTarget = nil
	inprocMu.Unlock()
}


//export goCamFrame
func goCamFrame(rgb *C.uint8_t, n C.int) {
	inprocMu.Lock()
	cs := inprocTarget
	inprocMu.Unlock()
	if cs == nil {
		return
	}
	cs.pushInprocFrame(C.GoBytes(unsafe.Pointer(rgb), n)) // GoBytes 已是新副本，可直接交给 CameraSource
}
