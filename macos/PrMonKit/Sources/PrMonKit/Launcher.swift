// Runs pr-mon commands through the user's login shell, so PATH matches the terminal.

import Foundation

public enum Launcher {
    public struct Failure: Error, LocalizedError {
        public var output: String
        public var errorDescription: String? { output }
    }

    /// `pr-mon start`: starts the backend if it isn't running.
    public static func startBackend() async throws {
        try await run("pr-mon start")
    }

    /// `pr-mon restart`: keeps it under launchd when autostart manages it.
    public static func restartBackend() async throws {
        try await run("pr-mon restart")
    }

    public static func stopBackend() async throws {
        try await run("pr-mon stop")
    }

    /// Whether the pr-mon command line is installed (the app shells out to it).
    public static func commandAvailable() async -> Bool {
        await output("command -v pr-mon").status == 0
    }

    public enum AutostartState: Sendable {
        case enabled, disabled
        /// The pr-mon command line isn't installed, so this can't be read or changed.
        case unavailable
    }

    /// Whether the backend starts at login (`pr-mon autostart status`).
    public static func autostartState() async -> AutostartState {
        let result = await output("pr-mon autostart status")
        if result.status == 127 || result.text.contains("command not found") {
            return .unavailable
        }
        return result.status == 0 ? .enabled : .disabled
    }

    public static func setAutostart(_ enabled: Bool) async throws {
        try await run("pr-mon autostart \(enabled ? "enable" : "disable")")
    }

    static func loginShell() -> String {
        if let entry = getpwuid(getuid()), let shell = entry.pointee.pw_shell {
            return String(cString: shell)
        }
        return "/bin/zsh"
    }

    static func run(_ command: String) async throws {
        let result = await output(command)
        guard result.status == 0 else {
            throw Failure(output: result.text.isEmpty ? "`\(command)` failed" : result.text)
        }
    }

    /// Runs a command and returns its exit status and combined output.
    static func output(_ command: String) async -> (status: Int32, text: String) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: loginShell())
        process.arguments = ["-l", "-c", command]
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
