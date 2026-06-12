//go:build !windows

package electronbot

import "github.com/ebitengine/purego"

// dlopen 在类 Unix 平台（Linux / 树莓派 / macOS）通过 purego 的 dlopen 加载共享库。
func dlopen(name string) (uintptr, error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}
