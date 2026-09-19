// Runs pr-mon commands: the copy inside the app bundle when there is one, else
// whatever the user's login shell finds on PATH (a source checkout).

import Foundation
import ServiceManagement

public enum Launcher {
    /// The backend's launchd agent shipped in the app bundle. Registering it
    /// through ServiceManagement is what makes Login Items say "PrMon" and show
    /// its icon: a plain agent in ~/Library/LaunchAgents has no bundle to name,
    /// so macOS falls back to whoever signed the binary.
    static let agentPlistName = "com.acheris-labs.pr-mon.plist"

    /// Non-nil when this build carries the agent, i.e. anything but a source
    /// checkout running the command line on its own.
    static func bundledAgent() -> SMAppService? {
        guard let path = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Library/LaunchAgents")
            .appendingPathComponent(agentPlistName).path as String?,
            FileManager.default.fileExists(atPath: path) else { return nil }
        return SMAppService.agent(plistName: agentPlistName)
    }
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
        /// Registered, but waiting for the user to allow it in Login Items.
        case needsApproval
        /// pr-mon isn't installed, so this can't be read or changed.
        case unavailable
    }

    /// Whether the backend starts at login.
    public static func autostartState() async -> AutostartState {
        if let agent = bundledAgent() {
            switch agent.status {
            case .enabled: return .enabled
            case .requiresApproval: return .needsApproval
            default: return .disabled
            }
        }
        let result = await output(["autostart", "status"])
        if result.status == 127 || result.text.contains("command not found") {
            return .unavailable
        }
        return result.status == 0 ? .enabled : .disabled
    }

    /// `pr-mon autostart`: the one place that knows how to hand the backend to
    /// launchd. From the bundle it calls back into this app's `--login-agent`.
    public static func setAutostart(_ enabled: Bool) async throws {
        try await run(["autostart", enabled ? "enable" : "disable"])
    }

    /// `PrMon --login-agent on|off|status`, run by the bundled command line:
    /// only this app can register the agent it ships. Prints the state it
    /// leaves (enabled, disabled or requires-approval) and returns the exit
    /// status, or nil when the arguments don't ask for this.
    public static func loginAgentCommand(_ arguments: [String]) -> Int32? {
        guard let flag = arguments.firstIndex(of: "--login-agent") else { return nil }
        let action = arguments.indices.contains(flag + 1) ? arguments[flag + 1] : ""
        guard let agent = bundledAgent() else {
            FileHandle.standardError.write(Data("this PrMon has no bundled agent\n".utf8))
            return 1
        }
        do {
            switch action {
            case "on": try agent.register()
            case "off": try agent.unregister()
            case "status": break
            default:
                FileHandle.standardError.write(Data("usage: PrMon --login-agent on|off|status\n".utf8))
                return 2
            }
        } catch {
            FileHandle.standardError.write(Data("\(error.localizedDescription)\n".utf8))
            return 1
        }
        switch agent.status {
        case .enabled: print("enabled")
        case .requiresApproval: print("requires-approval")
        default: print("disabled")
        }
        return 0
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
