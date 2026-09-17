// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "PrMonKit",
    platforms: [.macOS(.v14)],
    products: [
        .library(name: "PrMonKit", targets: ["PrMonKit"]),
    ],
    targets: [
        .target(name: "PrMonKit"),
        .testTarget(name: "PrMonKitTests", dependencies: ["PrMonKit"]),
    ]
)
