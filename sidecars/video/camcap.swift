// camcap —— 原生 macOS 摄像头采集(AVFoundation，不依赖 ffmpeg)。
// 用法: camcap "<摄像头名子串>" [边长]
//   按名字子串选摄像头(避开内建 FaceTime，选设备的 USB 摄像头)，采集每帧缩放为
//   边长×边长 的 RGB24，连续裸写到 stdout(每帧 边长*边长*3 字节)，供 Go 侧按帧长读取。
// 受“主程序无 cgo”约束，原生采集做成这个独立 Swift 小工具(类似 playto/musicto)，
// 由 Go 后端在 camera.backend=native 时以子进程方式调用。
import AVFoundation
import Foundation

let nameSub = CommandLine.arguments.count >= 2 ? CommandLine.arguments[1] : ""
let side = CommandLine.arguments.count >= 3 ? (Int(CommandLine.arguments[2]) ?? 240) : 240

func err(_ m: String) { FileHandle.standardError.write((m + "\n").data(using: .utf8)!) }

// 选摄像头：优先名字含子串、且【非】内建(避开 FaceTime)；否则第一个外接；再否则任意。
func pickCamera() -> AVCaptureDevice? {
    let types: [AVCaptureDevice.DeviceType] = [.builtInWideAngleCamera, .externalUnknown]
    let devs = AVCaptureDevice.DiscoverySession(deviceTypes: types, mediaType: .video, position: .unspecified).devices
    for d in devs where !nameSub.isEmpty && d.localizedName.contains(nameSub) { return d }
    // 名字没匹配上：挑一个外接(非内建 FaceTime)的
    for d in devs where !d.localizedName.contains("FaceTime") { return d }
    return devs.first
}

guard let cam = pickCamera() else { err("camcap: 没找到摄像头"); exit(2) }
err("camcap: 使用摄像头『\(cam.localizedName)』，输出 \(side)x\(side) rgb24")

final class Handler: NSObject, AVCaptureVideoDataOutputSampleBufferDelegate {
    let side: Int
    let out = FileHandle.standardOutput
    init(_ side: Int) { self.side = side }
    func captureOutput(_ output: AVCaptureOutput, didOutput sampleBuffer: CMSampleBuffer, from connection: AVCaptureConnection) {
        guard let pb = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }
        CVPixelBufferLockBaseAddress(pb, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(pb, .readOnly) }
        guard let base = CVPixelBufferGetBaseAddress(pb)?.assumingMemoryBound(to: UInt8.self) else { return }
        let w = CVPixelBufferGetWidth(pb), h = CVPixelBufferGetHeight(pb)
        let bpr = CVPixelBufferGetBytesPerRow(pb)
        var rgb = [UInt8](repeating: 0, count: side * side * 3) // BGRA → 最近邻缩放到 side×side → RGB24
        rgb.withUnsafeMutableBufferPointer { rp in          // 用裸指针，去掉数组越界检查，提速
            var o = 0
            for y in 0..<side {
                let row = (y * h / side) * bpr
                for x in 0..<side {
                    let p = row + (x * w / side) * 4
                    rp[o] = base[p + 2]     // R (BGRA 的 R 在 +2)
                    rp[o + 1] = base[p + 1] // G
                    rp[o + 2] = base[p]     // B
                    o += 3
                }
            }
        }
        out.write(Data(rgb))
    }
}

let session = AVCaptureSession()
do {
    let input = try AVCaptureDeviceInput(device: cam)
    if session.canAddInput(input) { session.addInput(input) }
} catch { err("camcap: 打开摄像头失败(可能没相机权限): \(error)"); exit(3) }
// 选低分辨率(≥side 且≤360宽)、支持≥30fps 的格式：USB2.0 带宽下小图才跑得满 30fps，缩放也更快。
do {
    try cam.lockForConfiguration()
    var best: AVCaptureDevice.Format?
    for f in cam.formats {
        let d = CMVideoFormatDescriptionGetDimensions(f.formatDescription)
        let fps = f.videoSupportedFrameRateRanges.map { $0.maxFrameRate }.max() ?? 0
        if d.width >= Int32(side) && d.width <= 360 && fps >= 30 { best = f; break }
    }
    if let b = best {
        cam.activeFormat = b
        let d = CMVideoFormatDescriptionGetDimensions(b.formatDescription)
        err("camcap: 选用采集格式 \(d.width)x\(d.height)")
        // 用格式自带的精确最小帧时长(对应其最高帧率)，避免设 1/30 因精度越界崩溃。
        if let r = b.videoSupportedFrameRateRanges.max(by: { $0.maxFrameRate < $1.maxFrameRate }) {
            cam.activeVideoMinFrameDuration = r.minFrameDuration
        }
    }
    cam.unlockForConfiguration()
} catch { err("camcap: 配置格式/帧率失败: \(error)") }

let output = AVCaptureVideoDataOutput()
output.videoSettings = [kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA]
output.alwaysDiscardsLateVideoFrames = true
let handler = Handler(side)
output.setSampleBufferDelegate(handler, queue: DispatchQueue(label: "camcap"))
if session.canAddOutput(output) { session.addOutput(output) }
session.startRunning()
RunLoop.main.run() // 持续采集，由父进程(Go 侧 exec.CommandContext 的 ctx 取消)杀掉
