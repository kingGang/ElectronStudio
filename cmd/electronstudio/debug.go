package main

// 运行时自省接口（仅回环地址可调，与 /api/shutdown 同样的限定）。
//
// 存在的理由是一次真实的教训：2026-07-17 设备从 USB 总线上消失后，程序进入"网页全无响应、
// /api/shutdown 超时、CPU 空转半个核"的状态。因为没有 goroutine 栈可看，只能靠读代码反复猜
// 是哪把锁——猜了五次都没定论，现场还随着重启没了。装上它之后，同样的卡死只需一条命令：
//
//	curl --noproxy '*' http://127.0.0.1:8099/debug/pprof/goroutine?debug=2
//
// 输出里每个 goroutine 的完整调用栈都在，谁堵在哪把锁上一目了然（Go 会直接标出
// "semacquire" / "sync.(*Mutex).Lock" 以及已阻塞的时长）。
//
// 安全性：pprof 会暴露内存/栈信息，故与 /api/shutdown 一样【只允许回环地址】——本机自用工具，
// 不给局域网里的其他机器看。

import (
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
)

// debugRoutes 挂载 pprof 端点（仅本机可调）。
func (a *app) debugRoutes(mux *http.ServeMux) {
	// 采样率：默认阻塞/互斥剖析是关的，开着才能回答"卡在哪把锁上、卡了多久"。
	// 取样比例保守（1/100 的互斥竞争、阻塞超过 10ms 才记），常态开销可忽略。
	runtime.SetBlockProfileRate(int(10 * 1000 * 1000)) // 纳秒：只记 >10ms 的阻塞
	runtime.SetMutexProfileFraction(100)

	h := func(name string, fn http.HandlerFunc) {
		mux.HandleFunc(name, func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil || !net.ParseIP(host).IsLoopback() {
				a.log.Warn("拒绝非本机的调试请求", "from", r.RemoteAddr, "path", r.URL.Path)
				http.Error(w, "仅允许本机调用", http.StatusForbidden)
				return
			}
			fn(w, r)
		})
	}
	h("/debug/pprof/", pprof.Index)          // 索引页；goroutine?debug=2 = 全部栈，卡死时先看这个
	h("/debug/pprof/cmdline", pprof.Cmdline) //
	h("/debug/pprof/profile", pprof.Profile) // CPU 剖析：回答"半个核空转在烧什么"
	h("/debug/pprof/symbol", pprof.Symbol)   //
	h("/debug/pprof/trace", pprof.Trace)     //
}
