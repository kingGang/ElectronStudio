//go:build !windows

package main

import "github.com/ebitengine/purego"

// dlopen 在类 Unix 平台（Linux / 树莓派 / macOS）通过 purego 的 dlopen 加载共享库。
// 与 internal/robot/electronbot/loader_unix.go 同构。
func dlopen(name string) (uintptr, error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}
