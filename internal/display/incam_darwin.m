//go:build darwin

// 进程内 AVFoundation 摄像头采集的 Objective-C 实现(配合 incam_darwin.go)。逻辑与 sidecars/video
// /camcap.swift 一致:按名选摄像头(避开 FaceTime)、选低分辨率≥30fps 格式、BGRA→RGB24 最近邻缩放、
// 每帧回调 goCamFrame。区别仅在于:这里是【进程内】运行,没有子进程、没有 stdout 管道。

#import <Foundation/Foundation.h>
#import <AVFoundation/AVFoundation.h>
#include "_cgo_export.h" // 提供 goCamFrame 声明(由 incam_darwin.go 的 //export 生成)

// 送屏画面的旋转(顺时针 0/90/180/270)与左右镜像。官方上位机对送设备的画面不做任何变换——它那台
// 摄像头是正装的；而精英版把摄像头横着装进头里，出图天生躺倒，故留成可配(camera.rotate/mirror)。
static int esRotate = 0;
static int esMirror = 0;

// BGRA 像素缓冲 → side×side RGB24(最近邻缩放 + 旋转/镜像)。
static void esConvertToRGB(CVImageBufferRef pb, int side, uint8_t* rgb) {
    CVPixelBufferLockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
    uint8_t* base = (uint8_t*)CVPixelBufferGetBaseAddress(pb);
    if (base) {
        int w   = (int)CVPixelBufferGetWidth(pb);
        int h   = (int)CVPixelBufferGetHeight(pb);
        int bpr = (int)CVPixelBufferGetBytesPerRow(pb);
        int o = 0;
        int last = side - 1;
        for (int y = 0; y < side; y++) {
            for (int x = 0; x < side; x++) {
                // 目标像素(x,y) → 未旋转方格里的(u,v)。取的是【逆变换】：要把源顺时针转 90°，
                // 目标的 (x,y) 就得去源的 (y, last-x) 处取样。
                int u, v;
                switch (esRotate) {
                case 90:  u = y;        v = last - x; break;
                case 180: u = last - x; v = last - y; break;
                case 270: u = last - y; v = x;        break;
                default:  u = x;        v = y;        break;
                }
                if (esMirror) u = last - u;
                int p = (v * h / side) * bpr + (u * w / side) * 4;
                rgb[o]     = base[p + 2];            // R (BGRA 的 R 在 +2)
                rgb[o + 1] = base[p + 1];            // G
                rgb[o + 2] = base[p];               // B
                o += 3;
            }
        }
    }
    CVPixelBufferUnlockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
}

// esCamSetOrientation 设置送屏画面的旋转/镜像(须在 esCamStart 之前调用)。
void esCamSetOrientation(int rotate, int mirror) {
    esRotate = ((rotate % 360) + 360) % 360;
    if (esRotate != 90 && esRotate != 180 && esRotate != 270) esRotate = 0;
    esMirror = mirror ? 1 : 0;
}

// ── 流式采集委托 ──
@interface ESCamDelegate : NSObject <AVCaptureVideoDataOutputSampleBufferDelegate>
@property (nonatomic) int side;
@end
@implementation ESCamDelegate
- (void)captureOutput:(AVCaptureOutput *)output
  didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
         fromConnection:(AVCaptureConnection *)connection {
    CVImageBufferRef pb = CMSampleBufferGetImageBuffer(sampleBuffer);
    if (!pb) return;
    int side = self.side;
    uint8_t* rgb = (uint8_t*)malloc((size_t)side * side * 3);
    if (rgb) {
        esConvertToRGB(pb, side, rgb);
        goCamFrame(rgb, side * side * 3);
        free(rgb);
    }
}
@end

static AVCaptureSession* gSession = nil;
static ESCamDelegate*   gDelegate = nil;
static dispatch_queue_t gQueue   = nil;

// 选摄像头:优先名字含子串、且非内建(避开 FaceTime);否则第一个非 FaceTime;再否则任意。
static AVCaptureDevice* esPickCamera(const char* nameSub) {
    NSString* sub = nameSub ? [NSString stringWithUTF8String:nameSub] : @"";
    NSArray* types = @[AVCaptureDeviceTypeBuiltInWideAngleCamera, AVCaptureDeviceTypeExternalUnknown];
    AVCaptureDeviceDiscoverySession* ds =
        [AVCaptureDeviceDiscoverySession discoverySessionWithDeviceTypes:types
                                                              mediaType:AVMediaTypeVideo
                                                               position:AVCaptureDevicePositionUnspecified];
    NSArray<AVCaptureDevice*>* devs = ds.devices;
    if (sub.length > 0)
        for (AVCaptureDevice* d in devs)
            if ([d.localizedName containsString:sub]) return d;
    for (AVCaptureDevice* d in devs)
        if (![d.localizedName containsString:@"FaceTime"]) return d;
    return devs.firstObject;
}

// 选低分辨率(≥side 且 ≤360 宽)、≥30fps 的格式。
static void esPickFormat(AVCaptureDevice* cam, int side) {
    NSError* err = nil;
    if ([cam lockForConfiguration:&err]) {
        AVCaptureDeviceFormat* best = nil;
        for (AVCaptureDeviceFormat* f in cam.formats) {
            CMVideoDimensions dim = CMVideoFormatDescriptionGetDimensions(f.formatDescription);
            double fps = 0;
            for (AVFrameRateRange* r in f.videoSupportedFrameRateRanges)
                if (r.maxFrameRate > fps) fps = r.maxFrameRate;
            if (dim.width >= side && dim.width <= 360 && fps >= 30) { best = f; break; }
        }
        if (best) {
            cam.activeFormat = best;
            AVFrameRateRange* r = best.videoSupportedFrameRateRanges.firstObject;
            if (r) cam.activeVideoMinFrameDuration = r.minFrameDuration;
        }
        [cam unlockForConfiguration];
    }
}

// 同步申请摄像头权限:返回 1=已授权。
int esCamAuth(void) {
    AVAuthorizationStatus st = [AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeVideo];
    if (st == AVAuthorizationStatusAuthorized) return 1;
    if (st == AVAuthorizationStatusNotDetermined) {
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block int ok = 0;
        [AVCaptureDevice requestAccessForMediaType:AVMediaTypeVideo completionHandler:^(BOOL granted){
            ok = granted ? 1 : 0;
            dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        return ok;
    }
    return 0;
}

// 启动【流式】采集:0=成功,负值=错误。side=输出边长(=屏幕 240)。
int esCamStart(const char* nameSub, int side) {
    @autoreleasepool {
        AVCaptureDevice* cam = esPickCamera(nameSub);
        if (!cam) return -1;
        NSError* err = nil;
        AVCaptureDeviceInput* input = [AVCaptureDeviceInput deviceInputWithDevice:cam error:&err];
        if (!input) return -2;
        esPickFormat(cam, side);

        AVCaptureSession* session = [[AVCaptureSession alloc] init];
        if (![session canAddInput:input]) return -3;
        [session addInput:input];

        AVCaptureVideoDataOutput* output = [[AVCaptureVideoDataOutput alloc] init];
        output.videoSettings = @{ (id)kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_32BGRA) };
        output.alwaysDiscardsLateVideoFrames = YES;
        ESCamDelegate* del = [[ESCamDelegate alloc] init];
        del.side = side;
        gQueue = dispatch_queue_create("es.camcap", DISPATCH_QUEUE_SERIAL);
        [output setSampleBufferDelegate:del queue:gQueue];
        if (![session canAddOutput:output]) return -4;
        [session addOutput:output];

        [session startRunning];
        gSession  = session; // 静态保活(ARC)
        gDelegate = del;
        return 0;
    }
}

void esCamStop(void) {
    @autoreleasepool {
        if (gSession) { [gSession stopRunning]; gSession = nil; }
        gDelegate = nil;
        gQueue = nil;
    }
}
