// Observable app state: connects, stays connected, and forwards commands.

import Foundation
import Observation

@MainActor
@Observable
public final class PrMonStore {
    public enum Phase: Equatable, Sendable {
        case connecting
        case startingBackend
        case connected
        /// Not connected; the message says why. Reconnecting continues in the background.
        case disconnected(String)
        /// The backend speaks another protocol; retrying won't help until one is updated.
        case incompatible(String)
    }

    public struct ToastItem: Identifiable, Equatable, Sendable {
        public let id = UUID()
        public var message: String
        public var severity: ToastSeverity
    }

    public private(set) var phase: Phase = .connecting
    public private(set) var state: BackendState? {
        didSet {
            let total = state?.unseenTotal ?? 0
            if total != lastUnseenTotal {
                lastUnseenTotal = total
                unseenTotalChanged?(total)
            }
        }
    }

    /// Called with the unseen-PR total whenever it changes. The app badges the
    /// Dock from here rather than from a view, which stops updating once its
    /// window is closed and the app keeps running.
    @ObservationIgnored public var unseenTotalChanged: ((Int) -> Void)?
    @ObservationIgnored private var lastUnseenTotal = 0
    public private(set) var toasts: [ToastItem] = []

    public let socketPath: String
    private let client: BackendClient
    private let retryDelay: Duration
    private let toastDuration: Duration
    private let startBackend: @Sendable () async throws -> Void
    private let stoppedOnPurpose: @Sendable () -> Bool
    private var loop: Task<Void, Never>?
    private var wake: CheckedContinuation<Void, Never>?
    /// Whether the backend has been started since the last good connection:
    /// each time one goes away it gets one restart, not a loop of them.
    private var triedStarting = false

    public init(
        socketPath: String = SocketPath.current(),
        retryDelay: Duration = .seconds(5),
        toastDuration: Duration = .seconds(6),
        startBackend: @escaping @Sendable () async throws -> Void = Launcher.startBackend,
        stoppedOnPurpose: @escaping @Sendable () -> Bool = {
            FileManager.default.fileExists(atPath: SocketPath.stoppedMarker())
        }
    ) {
        self.socketPath = socketPath
        self.client = BackendClient(socketPath: socketPath)
        self.retryDelay = retryDelay
        self.toastDuration = toastDuration
        self.startBackend = startBackend
        self.stoppedOnPurpose = stoppedOnPurpose
    }

    public var isConnected: Bool { phase == .connected }

    public func start() {
        guard loop == nil else { return }
        loop = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                let retryAtOnce = await self.runConnection()
                if !retryAtOnce {
                    await self.waitBeforeRetry()
                }
            }
        }
    }

    public func stop() {
        loop?.cancel()
        loop = nil
        retryNow()
        Task { await client.close() }
    }

    /// Skip the wait before the next connection attempt.
    public func retryNow() {
        wake?.resume()
        wake = nil
    }

    /// Runs `pr-mon start` (which returns once the backend answers) and reconnects.
    /// Returns whether the backend started.
    @discardableResult
    public func launchBackend() async -> Bool {
        phase = .startingBackend
        do {
            try await startBackend()
        } catch {
            phase = .disconnected("Couldn't start the backend: \(Self.cause(of: error.localizedDescription))")
            return false
        }
        phase = .connecting
        retryNow()
        return true
    }

    /// The one line worth showing from a failed `pr-mon start`. Its output is
    /// "backend exited with code N" and then the tail of the log; the backend's
    /// own error is the last line it printed with the "pr-mon: " prefix.
    static func cause(of output: String) -> String {
        let prefix = "pr-mon: "
        let lines = output.split(whereSeparator: \.isNewline)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
        let line = lines.last { $0.hasPrefix(prefix) && !$0.hasPrefix(prefix + "backend ") }
            ?? lines.first ?? output
        return line.hasPrefix(prefix) ? String(line.dropFirst(prefix.count)) : line
    }

    public func notify(_ message: String, severity: ToastSeverity = .information) {
        let item = ToastItem(message: message, severity: severity)
        toasts.append(item)
        let duration = severity == .error ? toastDuration * 2 : toastDuration
        Task { [weak self] in
            try? await Task.sleep(for: duration)
            self?.toasts.removeAll { $0.id == item.id }
        }
    }

    /// Returns true to reconnect without waiting.
    private func runConnection() async -> Bool {
        let wasConnected = state != nil
        if !wasConnected {
            phase = .connecting
        }
        let connection: Connection
        do {
            connection = try await client.connect()
        } catch ClientError.unavailable where !triedStarting {
            // Like the TUI, start the backend when the app opens and it isn't
            // running. One that went away while connected is restarted too --
            // an upgrade or a crash -- unless someone stopped it on purpose.
            if wasConnected && stoppedOnPurpose() {
                phase = .disconnected("The backend was stopped.")
                return false
            }
            triedStarting = true
            return await launchBackend()
        } catch let error as ClientError {
            if case .protocolMismatch = error {
                phase = .incompatible(error.localizedDescription)
            } else {
                phase = .disconnected(error.localizedDescription)
            }
            return false
        } catch {
            phase = .disconnected(error.localizedDescription)
            return false
        }
        triedStarting = false
        state = BackendState(snapshot: connection.snapshot)
        phase = .connected
        if wasConnected {
            notify("Backend reconnected")
        } else {
            for warning in connection.snapshot.status.warnings {
                notify(warning, severity: .warning)
            }
        }
        for await event in connection.events {
            if case let .toast(toast) = event {
                notify(toast.message, severity: toast.severity)
            }
            state?.apply(event)
        }
        phase = .disconnected("reconnecting…")
        notify("Backend disconnected — reconnecting…", severity: .warning)
        return false
    }

    private func waitBeforeRetry() async {
        await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
            wake = continuation
            let delay = retryDelay
            Task { [weak self] in
                try? await Task.sleep(for: delay)
                self?.retryNow()
            }
        }
    }

    // MARK: commands (results and GitHub failures arrive as toasts)

    public func refreshAll() {
        run { try await $0.refreshAll() }
    }

    public func markSeen(repo: String, number: Int) {
        guard state?.entry(repo).unseen.contains(number) == true else { return }
        state?.markSeenLocally(repo: repo, number: number)
        run { try await $0.markSeen(repo: repo, number: number) }
    }

    public func setCollapsed(owner: String, collapsed: Bool) {
        state?.setCollapsedLocally(owner: owner, collapsed: collapsed)
        run { try await $0.setCollapsed(owner: owner, collapsed: collapsed) }
    }

    public func perform(repo: String, number: Int, action: Action) {
        run { try await $0.perform(repo: repo, number: number, action: action) }
    }

    public func removeDependency(repo: String, number: Int, on: String) {
        run { try await $0.removeDependency(repo: repo, number: number, on: on) }
    }

    /// Throws with a message for the user, so a picker can show why.
    public func addDependency(repo: String, number: Int, on: String) async throws {
        try await client.addDependency(repo: repo, number: number, on: on)
    }

    public func dependencyGraph(repo: String, number: Int) async throws -> DependencyGraph {
        try await client.dependencyGraph(repo: repo, number: number)
    }

    public func setPollInterval(_ seconds: Int) {
        run { try await $0.setPollInterval(seconds) }
    }

    public func saveNotifications(repo: String, settings: NotifyConfig) {
        run { try await $0.saveNotifications(repo: repo, settings: settings) }
    }

    public func sendTest(repo: String, settings: NotifyConfig) {
        run { try await $0.sendTest(repo: repo, settings: settings) }
    }

    /// Throws with a message for the user; returns the name as GitHub spells it.
    public func addRepo(_ name: String) async throws -> String {
        try await client.addRepo(name)
    }

    public func removeRepo(_ name: String) async throws {
        try await client.removeRepo(name)
    }

    public func notificationForm() async throws -> NotificationForm {
        try await client.notificationForm()
    }

    public func previewNotification(repo: String, message: String) async throws
        -> NotificationPreview
    {
        try await client.previewNotification(repo: repo, message: message)
    }

    private func run(_ command: @escaping @Sendable (BackendClient) async throws -> Void) {
        let client = client
        Task {
            do {
                try await command(client)
            } catch {
                notify(error.localizedDescription, severity: .error)
            }
        }
    }
}
