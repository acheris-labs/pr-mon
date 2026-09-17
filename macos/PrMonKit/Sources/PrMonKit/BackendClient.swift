// A connection to the backend: requests with replies, plus pushed events.

import Foundation

public enum ClientError: Error, Equatable, LocalizedError {
    /// Nothing is listening on the socket.
    case unavailable
    case disconnected
    /// The backend speaks another protocol version.
    case protocolMismatch(backend: Int, version: String)
    /// The backend refused a request; the message is for the user.
    case backend(String)
    case malformed(String)
    case io(String)

    public var errorDescription: String? {
        switch self {
        case .unavailable: "The pr-mon backend isn't running"
        case .disconnected: "Lost the connection to the pr-mon backend"
        case let .protocolMismatch(backend, version):
            "The backend (pr-mon \(version)) speaks protocol \(backend); this app needs "
                + "\(protocolVersion). Update the one that is older."
        case let .backend(message): message
        case let .malformed(detail): "Unexpected data from the backend: \(detail)"
        case let .io(detail): detail
        }
    }
}

public struct Connection: Sendable {
    public var hello: Hello
    public var snapshot: Snapshot
    /// Events after the snapshot; finishes when the connection drops.
    public var events: AsyncStream<BackendEvent>
}

public actor BackendClient {
    private let socketPath: String
    private var socket: UnixSocket?
    private var nextID = 1
    private var pending: [Int: CheckedContinuation<Data, Error>] = [:]
    private var eventContinuation: AsyncStream<BackendEvent>.Continuation?

    public init(socketPath: String) {
        self.socketPath = socketPath
    }

    /// Connect, check the protocol version, and subscribe.
    public func connect() async throws -> Connection {
        close()
        let socket = try UnixSocket.connect(path: socketPath)
        self.socket = socket
        let (events, continuation) = AsyncStream.makeStream(
            of: BackendEvent.self, bufferingPolicy: .unbounded)
        eventContinuation = continuation
        let lines = socket.lines()
        Task { await self.readLoop(lines, socket: socket) }
        do {
            let hello: Hello = try await request("hello", NoArgs())
            let backendProtocol = hello.protocol ?? 0
            guard backendProtocol == protocolVersion else {
                throw ClientError.protocolMismatch(backend: backendProtocol, version: hello.version)
            }
            let snapshot: Snapshot = try await request("snapshot", NoArgs())
            return Connection(hello: hello, snapshot: snapshot, events: events)
        } catch {
            close()
            throw error
        }
    }

    public func close() {
        socket?.shutdown()
        socket = nil
        finish()
    }

    // MARK: commands

    public func refreshAll() async throws {
        try await command("refresh_all", NoArgs())
    }

    public func markSeen(repo: String, number: Int) async throws {
        try await command("mark_seen", RepoNumberArgs(name: repo, number: number))
    }

    public func perform(repo: String, number: Int, action: Action) async throws {
        try await command("perform", PerformArgs(repo: repo, number: number, action: action))
    }

    /// Returns the repo name as GitHub spells it.
    public func addRepo(_ name: String) async throws -> String {
        try await request("add_repo", NameArgs(name: name))
    }

    public func removeRepo(_ name: String) async throws {
        try await command("remove_repo", NameArgs(name: name))
    }

    public func setCollapsed(owner: String, collapsed: Bool) async throws {
        try await command("set_collapsed", CollapsedArgs(owner: owner, collapsed: collapsed))
    }

    public func saveNotifications(repo: String, settings: NotifyConfig) async throws {
        try await command("save_notifications", SettingsArgs(repo: repo, settings: settings))
    }

    public func sendTest(repo: String, settings: NotifyConfig) async throws {
        try await command("send_test", SettingsArgs(repo: repo, settings: settings))
    }

    public func notificationForm() async throws -> NotificationForm {
        try await request("notification_form", NoArgs())
    }

    public func previewNotification(repo: String, message: String) async throws -> NotificationPreview {
        try await request("preview_notification", PreviewArgs(repo: repo, message: message))
    }

    // MARK: plumbing

    private struct Ignored: Decodable {}

    private func command(_ op: String, _ args: some Encodable & Sendable) async throws {
        let _: Ignored? = try await request(op, args)
    }

    func request<T: Decodable>(_ op: String, _ args: some Encodable & Sendable) async throws -> T {
        guard let socket else { throw ClientError.disconnected }
        let id = nextID
        nextID += 1
        let data = try Wire.encode(Request(id: id, op: op, args: args))
        let line = try await withCheckedThrowingContinuation { continuation in
            pending[id] = continuation
            do {
                try socket.write(data)
            } catch {
                pending.removeValue(forKey: id)?.resume(throwing: error)
            }
        }
        do {
            return try JSONDecoder().decode(ResultBox<T>.self, from: line).result
        } catch {
            throw ClientError.malformed("\(op): \(error)")
        }
    }

    private func readLoop(_ lines: AsyncStream<Data>, socket: UnixSocket) async {
        for await line in lines {
            guard self.socket === socket else { break }
            handle(line)
        }
        if self.socket === socket {
            self.socket = nil
            finish()
        }
    }

    private func handle(_ line: Data) {
        guard let envelope = try? JSONDecoder().decode(Envelope.self, from: line) else {
            return
        }
        if let name = envelope.event {
            if let event = try? Wire.decodeEvent(name, from: line) {
                eventContinuation?.yield(event)
            }
            return
        }
        guard let id = envelope.id, let continuation = pending.removeValue(forKey: id) else {
            return
        }
        if envelope.ok == true {
            continuation.resume(returning: line)
        } else {
            continuation.resume(throwing: ClientError.backend(envelope.error ?? "request failed"))
        }
    }

    private func finish() {
        for continuation in pending.values {
            continuation.resume(throwing: ClientError.disconnected)
        }
        pending.removeAll()
        eventContinuation?.finish()
        eventContinuation = nil
    }
}
