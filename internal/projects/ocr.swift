// OCR helper for gostafson projects: reads text from images and from PDF
// pages with macOS's Vision framework. Compiled with swiftc on first use.
//
// Arguments, one per item:   img:<path>   pdf:<page>:<path>  (page from 1)
// Output: one JSON object per item and line: {"item", "text"} or {"item", "error"}.
import AppKit
import Foundation
import PDFKit
import Vision

func recognize(_ image: CGImage) -> String {
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = .accurate
    request.usesLanguageCorrection = true
    let handler = VNImageRequestHandler(cgImage: image, options: [:])
    do { try handler.perform([request]) } catch { return "" }
    return (request.results ?? []).compactMap { $0.topCandidates(1).first?.string }.joined(separator: "\n")
}

func emit(_ item: String, text: String? = nil, error: String? = nil) {
    var out: [String: String] = ["item": item]
    if let text { out["text"] = text }
    if let error { out["error"] = error }
    if let data = try? JSONSerialization.data(withJSONObject: out) {
        FileHandle.standardOutput.write(data)
        FileHandle.standardOutput.write(Data([0x0a]))
    }
}

// onWhite draws an image over white, so text in a PNG with a transparent
// background isn't read as black on black.
func onWhite(_ image: CGImage) -> CGImage? {
    guard let ctx = CGContext(data: nil, width: image.width, height: image.height, bitsPerComponent: 8, bytesPerRow: 0,
                              space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
    else { return nil }
    let rect = CGRect(x: 0, y: 0, width: image.width, height: image.height)
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
    ctx.fill(rect)
    ctx.draw(image, in: rect)
    return ctx.makeImage()
}

func renderPage(_ page: PDFPage) -> CGImage? {
    let box = page.bounds(for: .mediaBox)
    let scale: CGFloat = 2 // about 144 dpi: enough for manual text
    let width = Int(box.width * scale), height = Int(box.height * scale)
    guard width > 0, height > 0,
          let ctx = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                              space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
    else { return nil }
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
    ctx.fill(CGRect(x: 0, y: 0, width: width, height: height))
    ctx.scaleBy(x: scale, y: scale)
    page.draw(with: .mediaBox, to: ctx)
    return ctx.makeImage()
}

for arg in CommandLine.arguments.dropFirst() {
    if arg.hasPrefix("img:") {
        let url = URL(fileURLWithPath: String(arg.dropFirst(4)))
        guard let src = CGImageSourceCreateWithURL(url as CFURL, nil),
              let image = CGImageSourceCreateImageAtIndex(src, 0, nil)
        else { emit(arg, error: "can't read the image"); continue }
        emit(arg, text: recognize(onWhite(image) ?? image))
    } else if arg.hasPrefix("pdf:") {
        let parts = arg.dropFirst(4).split(separator: ":", maxSplits: 1)
        guard parts.count == 2, let number = Int(parts[0]),
              let doc = PDFDocument(url: URL(fileURLWithPath: String(parts[1]))),
              let page = doc.page(at: number - 1)
        else { emit(arg, error: "can't open that page"); continue }
        guard let image = renderPage(page) else { emit(arg, error: "can't render the page"); continue }
        emit(arg, text: recognize(image))
    } else {
        emit(arg, error: "unknown item")
    }
}
