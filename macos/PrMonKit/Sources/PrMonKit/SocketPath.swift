// Where the backend listens (docs/protocol.md, "Transport").

import CryptoKit
import Foundation

public enum SocketPath {
    /// macOS limits Unix socket paths to 104 bytes (including the terminator).
    static let limit = 100

    /// The backend's state directory: `$XDG_STATE_HOME/pr-mon` or `~/.local/state/pr-mon`.
    public static func stateDirectory(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        home: String = NSHomeDirectory()
    ) -> String {
        if let base = environment["XDG_STATE_HOME"], !base.isEmpty {
            return (base as NSString).appendingPathComponent("pr-mon")
        }
        return (home as NSString).appendingPathComponent(".local/state/pr-mon")
    }

    public static func socket(stateDirectory: String, uid: uid_t = getuid()) -> String {
        let path = (stateDirectory as NSString).appendingPathComponent("daemon.sock")
        if path.utf8.count <= limit {
            return path
        }
        // Too long to bind: the backend uses a private per-user directory instead.
        let digest = SHA256.hash(data: Data(stateDirectory.utf8))
        let hex = digest.map { String(format: "%02x", $0) }.joined()
        return "/tmp/pr-mon-\(uid)/\(hex.prefix(16)).sock"
    }

    public static func current() -> String {
        socket(stateDirectory: stateDirectory())
    }

    /// Present while the backend is down because someone asked it to stop
    /// (docs/protocol.md, "Starting the backend").
    public static func stoppedMarker(stateDirectory: String = stateDirectory()) -> String {
        (stateDirectory as NSString).appendingPathComponent("stopped")
    }
}
