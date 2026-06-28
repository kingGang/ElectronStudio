// playto —— 把 stdin 的音频(mp3/wav/aiff)播放到【指定输出设备】，不改系统默认输出。
// 用法: playto "<设备名子串>"   （音频字节从 stdin 进）
// 例:   cat tts.mp3 | playto "USB audio CODEC"
//
// 受“主程序无 cgo”约束，原生音频能力做成这个独立 Swift 小工具(类似 sidecar)，
// 由 Go 后端在 audio_out=device 时以子进程方式调用。靠 NSSound.playbackDeviceIdentifier
// 把单段声音定向到该设备，绝不动用户的系统默认输出。
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

func err(_ m: String) { FileHandle.standardError.write((m + "\n").data(using: .utf8)!) }

// 把设备输出音量拉满（主元素 0 + 左右声道 1/2，按设备支持且可设置的来）。USB 声卡默认音量常偏小。
func setVolume(_ id: AudioDeviceID, _ vol: Float32) {
    for el: AudioObjectPropertyElement in [0, 1, 2] {
        var a = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyVolumeScalar, mScope: kAudioObjectPropertyScopeOutput, mElement: el)
        if AudioObjectHasProperty(id, &a) {
            var settable: DarwinBoolean = false
            AudioObjectIsPropertySettable(id, &a, &settable)
            if settable.boolValue {
                var v = vol
                AudioObjectSetPropertyData(id, &a, 0, nil, UInt32(MemoryLayout<Float32>.size), &v)
            }
        }
    }
}

// 用法: playto <设备名子串> [音量0-100]   (音频从 stdin 进；stdin 为空时只设音量不播)
guard CommandLine.arguments.count >= 2 else { err("usage: playto <device-name-substr> [volume0-100]  (audio on stdin)"); exit(2) }
let sub = CommandLine.arguments[1]
// 可选音量参数：给了就设设备输出音量；没给就【不动】当前音量（这样滑块设过的值能保持）。
let volArg: Float32? = CommandLine.arguments.count >= 3 ? Float(CommandLine.arguments[2]).map { Float32(max(0, min(100, $0)) / 100) } : nil

// 找到名字含子串、且有输出声道的设备，取其 UID。给了音量参数则设音量。
var targetUID: String? = nil
for d in deviceIDs() where outChannels(d) > 0 {
    let nm = cfStr(d, kAudioObjectPropertyName)
    if nm.contains(sub) {
        targetUID = cfStr(d, kAudioDevicePropertyDeviceUID)
        if let v = volArg { setVolume(d, v) }
        break
    }
}
if targetUID == nil { err("playto: 未找到含『\(sub)』的输出设备，改用系统默认输出") }

// stdin -> 临时文件（NSSound 需要文件/URL）。
let data = FileHandle.standardInput.readDataToEndOfFile()
if data.isEmpty { exit(0) }
let tmp = NSTemporaryDirectory() + "ebplay-\(getpid())-\(Int(Date().timeIntervalSince1970)).audio"
do { try data.write(to: URL(fileURLWithPath: tmp)) } catch { err("playto: 写临时文件失败: \(error)"); exit(3) }
defer { try? FileManager.default.removeItem(atPath: tmp) }

guard let sound = NSSound(contentsOfFile: tmp, byReference: false) else { err("playto: 无法解码音频"); exit(4) }
if let u = targetUID { sound.playbackDeviceIdentifier = u }
if !sound.play() { err("playto: 播放启动失败"); exit(5) }
// 跑 run loop 让异步播放推进，按时长 + 余量等待。
let secs = sound.duration > 0 ? sound.duration : 30
RunLoop.current.run(until: Date().addingTimeInterval(secs + 0.5))
