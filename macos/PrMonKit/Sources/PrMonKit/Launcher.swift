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

    static func loginShell() -> String {
        if let entry = getpwuid(getuid()), let shell = entry.pointee.pw_shell {
            return String(cString: shell)
        }
        return "/bin/zsh"
    }

    static func run(_ command: String) async throws {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: loginShell())
        process.arguments = ["-l", "-c", command]
        let output = Pipe()
        process.standardOutput = output
        process.standardError = output
        process.standardInput = FileHandle.nullDevice
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            process.terminationHandler = { finished in
                let data = output.fileHandleForReading.readDataToEndOfFile()
                if finished.terminationStatus == 0 {
                    continuation.resume()
                } else {
                    let text = String(decoding: data, as: UTF8.self)
                        .trimmingCharacters(in: .whitespacesAndNewlines)
                    continuation.resume(throwing: Failure(
                        output: text.isEmpty ? "`\(command)` failed" : text))
                }
            }
            do {
                try process.run()
            } catch {
                continuation.resume(throwing: error)
            }
        }
    }
}
