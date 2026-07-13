//go:build windows

package main

import "syscall"

// dlopen 在 Windows 通过 LoadLibrary 加载 DLL（purego 在 Windows 上不提供 Dlopen——
// 直接调 purego.Dlopen 会编译不过，这也是本工具此前在 Windows 上一直构建失败的原因）。
// 与 internal/robot/electronbot/loader_windows.go 同构。
func dlopen(name string) (uintptr, error) {
	h, err := syscall.LoadLibrary(name)
	if err != nil {
		return 0, err
	}
	return uintptr(h), nil
}
