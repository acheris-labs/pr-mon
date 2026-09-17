#!/usr/bin/env swift
// Generates pr-mon's iconset. Usage:
//   swift tools/gen-icon.swift <iconset-dir> [design]
//   swift tools/gen-icon.swift --preview <dir>     # one 512px PNG per design
//
// Designs: dots (default), merge, pull, graph, graph-green, seal, watch, prompt.

import AppKit
import Foundation

struct Design {
    var name: String
    var top: NSColor
    var bottom: NSColor
    var draw: (CGFloat) -> Void
}

func color(_ red: Int, _ green: Int, _ blue: Int) -> NSColor {
    NSColor(srgbRed: CGFloat(red) / 255, green: CGFloat(green) / 255,
            blue: CGFloat(blue) / 255, alpha: 1)
}

let ink = color(244, 248, 245)

/// symbol draws an SF Symbol centred, filling `fraction` of the icon.
func symbol(_ name: String, size: CGFloat, fraction: CGFloat, color: NSColor = ink,
            offset: CGPoint = .zero) {
    let sizeConfig = NSImage.SymbolConfiguration(pointSize: size * fraction, weight: .semibold)
    let paletteConfig = NSImage.SymbolConfiguration(paletteColors: [color])
    guard let image = NSImage(systemSymbolName: name, accessibilityDescription: nil)?
        .withSymbolConfiguration(sizeConfig.applying(paletteConfig)) else { return }
    let drawn = image.size
    image.draw(in: NSRect(x: (size - drawn.width) / 2 + offset.x * size,
                          y: (size - drawn.height) / 2 + offset.y * size,
                          width: drawn.width, height: drawn.height))
}

/// gitGraph draws a branch curving back into a trunk, with a status dot at the tip.
func gitGraph(size: CGFloat, accent: NSColor) {
    let line = size * 0.075
    let trunkX = size * 0.34
    let branchX = size * 0.68

    let trunk = NSBezierPath()
    trunk.move(to: NSPoint(x: trunkX, y: size * 0.18))
    trunk.line(to: NSPoint(x: trunkX, y: size * 0.82))
    trunk.lineWidth = line
    trunk.lineCapStyle = .round
    ink.setStroke()
    trunk.stroke()

    // The branch leaves the trunk, runs up, and merges back in near the top.
    let branch = NSBezierPath()
    branch.move(to: NSPoint(x: trunkX, y: size * 0.34))
    branch.curve(to: NSPoint(x: branchX, y: size * 0.52),
                 controlPoint1: NSPoint(x: size * 0.52, y: size * 0.34),
                 controlPoint2: NSPoint(x: branchX, y: size * 0.40))
    branch.curve(to: NSPoint(x: trunkX, y: size * 0.70),
                 controlPoint1: NSPoint(x: branchX, y: size * 0.64),
                 controlPoint2: NSPoint(x: size * 0.52, y: size * 0.70))
    branch.lineWidth = line
    branch.lineCapStyle = .round
    branch.stroke()

    for centre in [NSPoint(x: trunkX, y: size * 0.34), NSPoint(x: trunkX, y: size * 0.70)] {
        let radius = size * 0.075
        let dot = NSBezierPath(ovalIn: NSRect(x: centre.x - radius, y: centre.y - radius,
                                              width: radius * 2, height: radius * 2))
        ink.setFill()
        dot.fill()
    }
    // The status dot on the branch tip: the "ready" signal pr-mon watches for.
    let radius = size * 0.105
    let tip = NSPoint(x: branchX, y: size * 0.52)
    let status = NSBezierPath(ovalIn: NSRect(x: tip.x - radius, y: tip.y - radius,
                                             width: radius * 2, height: radius * 2))
    accent.setFill()
    status.fill()
}

/// watchTower draws an eye over a merge arrow: the backend watching for readiness.
func watchTower(size: CGFloat) {
    symbol("eye.fill", size: size, fraction: 0.44, color: ink, offset: CGPoint(x: 0, y: 0.13))
    symbol("arrow.triangle.merge", size: size, fraction: 0.40,
           color: color(150, 230, 170), offset: CGPoint(x: 0, y: -0.17))
}


/// statusDots draws the three states pr-mon reports, as rows on a branch line.
/// Small sizes drop the rows and keep three larger dots, which stays legible in
/// the menu bar and the Dock's smallest sizes.
func statusDots(size: CGFloat) {
    if size <= 48 {
        smallDots(size: size)
        return
    }
    let line = size * 0.055
    let x = size * 0.30
    let trunk = NSBezierPath()
    trunk.move(to: NSPoint(x: x, y: size * 0.20))
    trunk.line(to: NSPoint(x: x, y: size * 0.80))
    trunk.lineWidth = line
    trunk.lineCapStyle = .round
    ink.withAlphaComponent(0.55).setStroke()
    trunk.stroke()

    for (index, dotColor) in statusColors.enumerated() {
        let centre = NSPoint(x: x, y: size * (0.72 - CGFloat(index) * 0.22))
        let radius = size * 0.085
        NSBezierPath(ovalIn: NSRect(x: centre.x - radius, y: centre.y - radius,
                                    width: radius * 2, height: radius * 2)).fill(with: dotColor)
        // A row out to the right, longest at the top: a list of pull requests.
        let row = NSBezierPath()
        row.move(to: NSPoint(x: centre.x + radius * 1.7, y: centre.y))
        row.line(to: NSPoint(x: size * (0.80 - CGFloat(index) * 0.11), y: centre.y))
        row.lineWidth = line
        row.lineCapStyle = .round
        ink.withAlphaComponent(0.85).setStroke()
        row.stroke()
    }
}

/// smallDots is the same idea with the rows dropped: three dots, stacked.
func smallDots(size: CGFloat) {
    let radius = size * 0.135
    for (index, dotColor) in statusColors.enumerated() {
        let centre = NSPoint(x: size * 0.5, y: size * (0.755 - CGFloat(index) * 0.255))
        NSBezierPath(ovalIn: NSRect(x: centre.x - radius, y: centre.y - radius,
                                    width: radius * 2, height: radius * 2)).fill(with: dotColor)
    }
}

let statusColors = [color(90, 220, 130), color(240, 190, 80), color(230, 100, 100)]

/// prompt draws a terminal chevron with a ready dot: the dashboard in a shell.
func prompt(size: CGFloat) {
    let chevron = NSBezierPath()
    chevron.move(to: NSPoint(x: size * 0.28, y: size * 0.30))
    chevron.line(to: NSPoint(x: size * 0.52, y: size * 0.50))
    chevron.line(to: NSPoint(x: size * 0.28, y: size * 0.70))
    chevron.lineWidth = size * 0.085
    chevron.lineCapStyle = .round
    chevron.lineJoinStyle = .round
    ink.setStroke()
    chevron.stroke()

    let radius = size * 0.085
    NSBezierPath(ovalIn: NSRect(x: size * 0.66 - radius, y: size * 0.50 - radius,
                                width: radius * 2, height: radius * 2))
        .fill(with: color(90, 220, 130))
}

extension NSBezierPath {
    func fill(with color: NSColor) {
        color.setFill()
        fill()
    }
}

let designs: [Design] = [
    Design(name: "merge", top: color(52, 140, 80), bottom: color(20, 72, 44)) { size in
        symbol("arrow.triangle.merge", size: size, fraction: 0.62)
    },
    Design(name: "pull", top: color(104, 96, 214), bottom: color(48, 40, 128)) { size in
        symbol("arrow.triangle.pull", size: size, fraction: 0.60)
    },
    Design(name: "graph", top: color(58, 66, 82), bottom: color(24, 28, 38)) { size in
        gitGraph(size: size, accent: color(90, 220, 130))
    },
    Design(name: "watch", top: color(38, 96, 150), bottom: color(14, 40, 78)) { size in
        watchTower(size: size)
    },
    Design(name: "graph-green", top: color(46, 132, 78), bottom: color(18, 66, 40)) { size in
        gitGraph(size: size, accent: color(236, 245, 238))
    },
    Design(name: "seal", top: color(60, 68, 84), bottom: color(26, 30, 40)) { size in
        symbol("checkmark.seal.fill", size: size, fraction: 0.66, color: color(90, 220, 130))
    },
    Design(name: "dots", top: color(52, 58, 72), bottom: color(22, 26, 34)) { size in
        statusDots(size: size)
    },
    Design(name: "prompt", top: color(34, 40, 52), bottom: color(14, 17, 24)) { size in
        prompt(size: size)
    },
]

/// sheet renders every design at a readable size with a 32px preview beside it,
/// so they can be compared at a glance.
func sheet() -> Data {
    let tile: CGFloat = 200
    let columns = 4
    let rows = (designs.count + columns - 1) / columns
    let width = CGFloat(columns) * tile
    let height = CGFloat(rows) * (tile + 34)
    let rep = NSBitmapImageRep(
        bitmapDataPlanes: nil, pixelsWide: Int(width), pixelsHigh: Int(height),
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bitmapFormat: [], bytesPerRow: 0, bitsPerPixel: 32)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    color(28, 28, 30).setFill()
    NSRect(x: 0, y: 0, width: width, height: height).fill()

    for (index, design) in designs.enumerated() {
        let column = CGFloat(index % columns)
        let row = CGFloat(index / columns)
        let left = column * tile
        let bottom = height - (row + 1) * (tile + 34) + 34

        let big = NSImage(size: NSSize(width: 128, height: 128))
        big.lockFocus()
        drawIcon(design, size: 128)
        big.unlockFocus()
        big.draw(in: NSRect(x: left + 16, y: bottom + 30, width: 128, height: 128))

        let small = NSImage(size: NSSize(width: 32, height: 32))
        small.lockFocus()
        drawIcon(design, size: 32)
        small.unlockFocus()
        small.draw(in: NSRect(x: left + 156, y: bottom + 30, width: 32, height: 32))
        small.draw(in: NSRect(x: left + 156, y: bottom + 70, width: 16, height: 16))

        let label = NSAttributedString(string: design.name, attributes: [
            .foregroundColor: NSColor.white,
            .font: NSFont.systemFont(ofSize: 15, weight: .medium),
        ])
        label.draw(at: NSPoint(x: left + 16, y: bottom + 6))
    }
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

func drawIcon(_ design: Design, size: CGFloat) {
    let rect = NSRect(x: 0, y: 0, width: size, height: size)
    let radius = size * 0.225
    let background = NSBezierPath(roundedRect: rect, xRadius: radius, yRadius: radius)
    NSGraphicsContext.current?.cgContext.saveGState()
    background.addClip()
    NSGradient(starting: design.top, ending: design.bottom)!.draw(in: rect, angle: 270)
    design.draw(size)
    NSGraphicsContext.current?.cgContext.restoreGState()
}

func png(_ design: Design, size: Int) -> Data {
    let rep = NSBitmapImageRep(
        bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bitmapFormat: [], bytesPerRow: 0, bitsPerPixel: 32)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    drawIcon(design, size: CGFloat(size))
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

let arguments = Array(CommandLine.arguments.dropFirst())
guard let first = arguments.first else {
    print("usage: gen-icon.swift <iconset-dir> [design] | --preview <dir>")
    exit(2)
}

if first == "--sheet" {
    let path = URL(fileURLWithPath: arguments.count > 1 ? arguments[1] : "icons.png")
    try sheet().write(to: path)
    print("wrote \(path.path)")
    exit(0)
}

if first == "--preview" {
    let directory = URL(fileURLWithPath: arguments.count > 1 ? arguments[1] : ".")
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    for design in designs {
        let path = directory.appendingPathComponent("icon-\(design.name).png")
        try png(design, size: 512).write(to: path)
        print("wrote \(path.path)")
    }
    exit(0)
}

let chosen = arguments.count > 1 ? arguments[1] : "dots"
guard let design = designs.first(where: { $0.name == chosen }) else {
    print("unknown design \(chosen); one of: \(designs.map(\.name).joined(separator: ", "))")
    exit(2)
}
let iconset = URL(fileURLWithPath: first)
try FileManager.default.createDirectory(at: iconset, withIntermediateDirectories: true)
// The sizes macOS expects in an iconset, each at 1x and 2x.
for size in [16, 32, 128, 256, 512] {
    try png(design, size: size).write(
        to: iconset.appendingPathComponent("icon_\(size)x\(size).png"))
    try png(design, size: size * 2).write(
        to: iconset.appendingPathComponent("icon_\(size)x\(size)@2x.png"))
}
print("wrote \(iconset.path) (\(design.name))")
