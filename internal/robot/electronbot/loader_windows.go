//go:build windows

package electronbot

import "syscall"

// dlopen 在 Windows 通过 LoadLibrary 加载 DLL（purego 在 Windows 上不提供 Dlopen）。
// 返回的句柄可直接交给 purego.RegisterLibFunc 解析导出函数。
func dlopen(name string) (uintptr, error) {
	h, err := syscall.LoadLibrary(name)
	if err != nil {
		return 0, err
	}
	return uintptr(h), nil
}
