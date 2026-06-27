// musicto —— 常驻音乐播放器，把音频文件播放到【指定输出设备】，不改系统默认输出。
// 用法: musicto "<设备名子串>"，然后从 stdin 逐行读命令(类似 mpg123 -R)：
//   LOAD <文件路径>   加载并播放(替换当前)
//   PAUSE / RESUME    暂停 / 恢复
//   STOP              停止
//   VOLUME <0-100>    音量
//   QUIT              退出
// 受“主程序无 cgo”约束，原生音频能力做成这个独立 Swift 小工具，由 Go 后端在
// audio_out=device 且 macOS 时以子进程方式调用。靠 NSSound.playbackDeviceIdentifier
// 把声音定向到该设备，绝不动用户的系统默认输出。
import AppKit
import CoreAudio
import Foundation

let sys = AudioObjectID(kAudioObjectSystemObject)

func cfStr(_ id: AudioObjectID, _ sel: AudioObjectPropertySelector) -> String {
    var a = AudioObjectPropertyAddress(mSelector: sel, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
    var v: CFString = "" as CFString; var s = UInt32(MemoryLayout<CFString>.size)
    AudioObjectGetPropertyData(id, &a, 0, nil, &s, &v); return v as String
}
func deviceIDs() -> [AudioDeviceID] {
    var a = AudioObjectPropertyAddress(mSelector: kAudioHardwarePropertyDevices, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
    var s = UInt32(0); AudioObjectGetPropertyDataSize(sys, &a, 0, nil, &s)
    var ids = [AudioDeviceID](repeating: 0, count: Int(s)/MemoryLayout<AudioDeviceID>.size)
    AudioObjectGetPropertyData(sys, &a, 0, nil, &s, &ids); return ids
}
func outChannels(_ id: AudioDeviceID) -> Int {
    var a = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyStreamConfiguration, mScope: kAudioObjectPropertyScopeOutput, mElement: kAudioObjectPropertyElementMain)
    var s = UInt32(0); AudioObjectGetPropertyDataSize(id, &a, 0, nil, &s)
    if s == 0 { return 0 }
    let p = UnsafeMutableRawPointer.allocate(byteCount: Int(s), alignment: 16); defer { p.deallocate() }
    AudioObjectGetPropertyData(id, &a, 0, nil, &s, p)
    let bl = UnsafeMutableAudioBufferListPointer(p.assumingMemoryBound(to: AudioBufferList.self))
    return bl.reduce(0) { $0 + Int($1.mNumberChannels) }
}
func setDeviceVolume(_ id: AudioDeviceID, _ vol: Float32) {
    for el: AudioObjectPropertyElement in [0, 1, 2] {
        var a = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyVolumeScalar, mScope: kAudioObjectPropertyScopeOutput, mElement: el)
        if AudioObjectHasProperty(id, &a) {
            var settable: DarwinBoolean = false
            AudioObjectIsPropertySettable(id, &a, &settable)
            if settable.boolValue { var v = vol; AudioObjectSetPropertyData(id, &a, 0, nil, UInt32(MemoryLayout<Float32>.size), &v) }
        }
    }
}
func err(_ m: String) { FileHandle.standardError.write((m + "\n").data(using: .utf8)!) }

let sub = CommandLine.arguments.count >= 2 ? CommandLine.arguments[1] : ""
var targetUID: String? = nil
for d in deviceIDs() where outChannels(d) > 0 {
    if !sub.isEmpty, cfStr(d, kAudioObjectPropertyName).contains(sub) {
        targetUID = cfStr(d, kAudioDevicePropertyDeviceUID)
        setDeviceVolume(d, 1.0) // 设备输出拉满，软件音量交给 NSSound.volume
        break
    }
}
if targetUID == nil { err("musicto: 未找到含『\(sub)』的输出设备，用系统默认") }

var current: NSSound?
var vol: Float = 0.8

func handle(_ line: String) {
    let parts = line.split(separator: " ", maxSplits: 1).map(String.init)
    guard let cmd = parts.first else { return }
    switch cmd {
    case "LOAD":
        guard parts.count >= 2 else { return }
        current?.stop()
        guard let s = NSSound(contentsOfFile: parts[1], byReference: false) else { err("musicto: 无法解码 \(parts[1])"); return }
        if let u = targetUID { s.playbackDeviceIdentifier = u }
        s.volume = vol
        current = s
        if !s.play() { err("musicto: 播放启动失败") }
    case "PAUSE": current?.pause()
    case "RESUME": current?.resume()
    case "STOP": current?.stop(); current = nil
    case "VOLUME":
        if parts.count >= 2, let v = Float(parts[1]) { vol = max(0, min(1, v / 100)); current?.volume = vol }
    case "QUIT": exit(0)
    default: break
    }
}

// stdin 读命令放后台线程，派发到主线程执行(NSSound 需主线程 + 运行循环)。
DispatchQueue.global().async {
    while let line = readLine() { DispatchQueue.main.async { handle(line) } }
    DispatchQueue.main.async { exit(0) } // stdin 关闭(主程序退出)→自己退出
}
RunLoop.main.run()
