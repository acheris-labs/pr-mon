// Runs pr-mon commands: the copy inside the app bundle when there is one, else
// whatever the user's login shell finds on PATH (a source checkout).

import Foundation

public enum Launcher {
    public struct Failure: Error, LocalizedError {
        public var output: String
        public var errorDescription: String? { output }
    }

    /// The pr-mon binary shipped inside this app bundle, if it is there.
    public static func bundledBinary() -> String? {
        guard let path = Bundle.main.resourceURL?
            .appendingPathComponent("bin/pr-mon").path else { return nil }
        return FileManager.default.isExecutableFile(atPath: path) ? path : nil
    }

    /// `pr-mon start`: starts the backend if it isn't running.
    public static func startBackend() async throws {
        try await run(["start"])
    }

    /// `pr-mon restart`: keeps it under launchd when autostart manages it.
    public static func restartBackend() async throws {
        try await run(["restart"])
    }

    public static func stopBackend() async throws {
        try await run(["stop"])
    }

    /// Whether pr-mon can be run at all: bundled with the app, or on PATH.
    public static func commandAvailable() async -> Bool {
        if bundledBinary() != nil {
            return true
        }
        return await shell("command -v pr-mon").status == 0
    }

    public enum AutostartState: Sendable {
        case enabled, disabled
        /// pr-mon isn't installed, so this can't be read or changed.
        case unavailable
    }

    /// Whether the backend starts at login (`pr-mon autostart status`).
    public static func autostartState() async -> AutostartState {
        let result = await output(["autostart", "status"])
        if result.status == 127 || result.text.contains("command not found") {
            return .unavailable
        }
        return result.status == 0 ? .enabled : .disabled
    }

    public static func setAutostart(_ enabled: Bool) async throws {
        try await run(["autostart", enabled ? "enable" : "disable"])
    }

    static func loginShell() -> String {
        if let entry = getpwuid(getuid()), let shell = entry.pointee.pw_shell {
            return String(cString: shell)
        }
        return "/bin/zsh"
    }

    static func run(_ arguments: [String]) async throws {
        let result = await output(arguments)
        guard result.status == 0 else {
            let command = "pr-mon " + arguments.joined(separator: " ")
            throw Failure(output: result.text.isEmpty ? "`\(command)` failed" : result.text)
        }
    }

    /// Runs pr-mon and returns its exit status and combined output.
    static func output(_ arguments: [String]) async -> (status: Int32, text: String) {
        if let binary = bundledBinary() {
            return await execute(binary, arguments)
        }
        // No bundled copy: the login shell knows where a source install put it.
        return await shell("pr-mon " + arguments.joined(separator: " "))
    }

    static func shell(_ command: String) async -> (status: Int32, text: String) {
        await execute(loginShell(), ["-l", "-c", command])
    }

    static func execute(_ path: String, _ arguments: [String]) async -> (status: Int32, text: String) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: path)
        process.arguments = arguments
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        process.standardInput = FileHandle.nullDevice
        return await withCheckedContinuation { continuation in
            process.terminationHandler = { finished in
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                let text = String(decoding: data, as: UTF8.self)
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                continuation.resume(returning: (finished.terminationStatus, text))
            }
            do {
                try process.run()
            } catch {
                continuation.resume(returning: (-1, error.localizedDescription))
            }
        }
    }
}
