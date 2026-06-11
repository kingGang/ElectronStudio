// Package web 通过 go:embed 把前端静态资源内嵌进二进制，
// 使最终产物为"单文件分发"——无需随附 web 目录。
//
// 前端为纯原生实现（无构建步骤、无外部依赖、可离线运行），位于 public/。
package web

import (
	"embed"
	"io/fs"
)

//go:embed public
var embedded embed.FS

// FS 返回以 public 为根的前端静态资源文件系统，供 http.FileServer 使用。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "public")
	if err != nil {
		// public 为编译期内嵌目录，正常情况下不会失败。
		panic(err)
	}
	return sub
}
