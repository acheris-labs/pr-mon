import Darwin
import Foundation
import Testing

@testable import PrMonKit

@Suite struct LineSplitterTests {
    @Test func splitsOnNewlineOnly() {
        var splitter = LineSplitter()
        #expect(splitter.append(Data("{\"a\":\"x\u{2028}y\"}\n{\"b\"".utf8))
            == [Data("{\"a\":\"x\u{2028}y\"}".utf8)])
        #expect(splitter.append(Data(":1}\n\n".utf8)) == [Data("{\"b\":1}".utf8)])
        #expect(splitter.append(Data()) == [])
    }
}

@Suite struct BackendStateTests {
    @Test func appliesEvents() throws {
        var state = BackendState(snapshot: try Fixtures.snapshot())
        #expect(state.repos.map(\.name) == ["acme/api", "acme/web", "Textualize/rich"])
        #expect(state.armed(repo: "Textualize/rich", number: 42)?.deleteBranch == true)

        state.apply(.repo(RepoUpdate(name: "acme/api", repo: nil, error: "boom", unseen: [], armed: [:])))
        #expect(state.entry("acme/api").repo == nil)
        #expect(state.entry("acme/api").error == "boom")

        state.apply(try Fixtures.event("repo"))
        #expect(state.entry("acme/api").error == nil)
        #expect(state.entry("acme/api").unseen == [1, 5])

        state.apply(.seen(SeenUpdate(name: "acme/api", unseen: [5])))
        #expect(state.entry("acme/api").unseen == [5])

        state.apply(.collapsed([]))
        #expect(state.snapshot.collapsed == [])

        state.apply(try Fixtures.event("repos"))
        #expect(state.snapshot == (try Fixtures.snapshot()))
    }

    @Test func localChanges() throws {
        var state = BackendState(snapshot: try Fixtures.snapshot())
        state.markSeenLocally(repo: "acme/api", number: 1)
        #expect(state.entry("acme/api").unseen == [5])
        state.setCollapsedLocally(owner: "acme", collapsed: true)
        #expect(state.snapshot.collapsed == ["acme", "textualize"])
        state.setCollapsedLocally(owner: "textualize", collapsed: false)
        #expect(state.snapshot.collapsed == ["acme"])
    }

    @Test func ownerGroups() throws {
        let state = BackendState(snapshot: try Fixtures.snapshot())
        let owners = state.owners
        #expect(owners.map(\.key) == ["acme", "textualize"])
        #expect(owners[0].repos.map(\.shortName) == ["api", "web"])
        #expect(owners[1].owner == "Textualize")
        #expect(owners[1].collapsed)
        #expect(owners[0].hasError)
        #expect(owners[0].unseenPRs.map(\.number) == [1, 5])
        #expect(BackendState.Badge(owners[0].unseenPRs) == .ready)
        #expect(BackendState.Badge([]) == nil)
        let api = state.entry("acme/api")
        #expect(api.count(.ready) == 1)
        #expect(api.alerts == 2)  // failing + conflict
    }
}

/// A one-connection backend double on a real Unix socket.
final class FakeServer: @unchecked Sendable {
    let path: String
    private let listener: Int32
    private var client: Int32 = -1
    private let directory: URL

    init() throws {
        directory = URL(fileURLWithPath: "/tmp").appendingPathComponent("prmonkit-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        path = directory.appendingPathComponent("d.sock").path
        listener = socket(AF_UNIX, SOCK_STREAM, 0)
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &address.sun_path) { buffer in
            let bytes = Array(path.utf8)
            buffer.copyBytes(from: bytes)
            buffer[bytes.count] = 0
        }
        let bound = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        precondition(bound == 0 && listen(listener, 1) == 0)
    }

    /// Accept the client, then answer requests with `respond` until it returns nil.
    func serve(_ respond: @escaping @Sendable (_ request: [String: Any], _ send: (Data) -> Void) -> Bool) {
        Thread { [self] in
            client = accept(listener, nil, nil)
            var splitter = LineSplitter()
            var buffer = [UInt8](repeating: 0, count: 65536)
            outer: while true {
                let count = buffer.withUnsafeMutableBytes { read(client, $0.baseAddress, $0.count) }
                if count <= 0 { break }
                for line in splitter.append(Data(buffer[0..<count])) {
                    let request = try! JSONSerialization.jsonObject(with: line) as! [String: Any]
                    if !respond(request, { self.send($0) }) { break outer }
                }
            }
            Darwin.shutdown(client, SHUT_RDWR)
        }.start()
    }

    func send(_ data: Data) {
        data.withUnsafeBytes { _ = write(client, $0.baseAddress, $0.count) }
    }

    static func reply(to request: [String: Any], fixture: String) throws -> Data {
        var object = try JSONSerialization.jsonObject(with: Fixtures.data(fixture)) as! [String: Any]
        object["id"] = request["id"]
        var data = try JSONSerialization.data(withJSONObject: object)
        data.append(0x0A)
        return data
    }

    deinit {
        close(listener)
        if client >= 0 { close(client) }
        try? FileManager.default.removeItem(at: directory)
    }
}

@Suite struct ClientTests {
    @Test func unavailable() async {
        let client = BackendClient(socketPath: "/tmp/prmonkit-missing.sock")
        await #expect(throws: ClientError.unavailable) { try await client.connect() }
    }

    @Test func connectsRequestsAndStreamsEvents() async throws {
        let server = try FakeServer()
        let seen = RequestLog()
        server.serve { request, send in
            let op = request["op"] as! String
            seen.append(op)
            switch op {
            case "hello": send(try! FakeServer.reply(to: request, fixture: "hello-response.json"))
            case "snapshot":
                send(try! FakeServer.reply(to: request, fixture: "snapshot-response.json"))
                send(try! Fixtures.line("event-toast.json"))
            case "perform":
                let action = (request["args"] as! [String: Any])["action"] as! [String: Any]
                #expect(action["kind"] as? String == "update")
                #expect(action["method"] is NSNull)
                send(try! FakeServer.reply(to: request, fixture: "error-response.json"))
                return false  // then hang up
            default:
                // No result key at all: a command must not need one.
                send(Data("{\"id\": \(request["id"]!), \"ok\": true}\n".utf8))
            }
            return true
        }
        let client = BackendClient(socketPath: server.path)
        let connection = try await client.connect()
        #expect(connection.hello.pid == 4242)
        #expect(connection.snapshot.repos.count == 2)
        var events = connection.events.makeAsyncIterator()
        #expect(await events.next() == .toast(Toast(message: "Merged acme/api#1 (squash)", severity: .information)))

        try await client.markSeen(repo: "acme/api", number: 1)
        await #expect(throws: ClientError.backend("acme/api#99 is not an open PR")) {
            try await client.perform(repo: "acme/api", number: 1, action: Action(kind: "update"))
        }
        #expect(await events.next() == nil)  // the server hung up
        await #expect(throws: ClientError.disconnected) { try await client.refreshAll() }
        #expect(seen.ops == ["hello", "snapshot", "mark_seen", "perform"])
    }

    @Test func protocolMismatchStopsBeforeSnapshot() async throws {
        let server = try FakeServer()
        let seen = RequestLog()
        server.serve { request, send in
            seen.append(request["op"] as! String)
            let id = request["id"]!
            send(Data(#"{"id": \#(id), "ok": true, "result": {"version": "0.0.9", "pid": 1, "notifier": null, "merging": false}}"#.utf8 + [0x0A]))
            return true
        }
        let client = BackendClient(socketPath: server.path)
        await #expect(throws: ClientError.protocolMismatch(backend: 0, version: "0.0.9")) {
            try await client.connect()
        }
        #expect(seen.ops == ["hello"])
    }
}

@MainActor
@Suite struct StoreTests {
    @Test func connectsAndShowsToasts() async throws {
        let server = try FakeServer()
        server.serve { request, send in
            switch request["op"] as! String {
            case "hello": send(try! FakeServer.reply(to: request, fixture: "hello-response.json"))
            case "snapshot":
                send(try! FakeServer.reply(to: request, fixture: "snapshot-response.json"))
                send(try! Fixtures.line("event-toast.json"))
            default:
                send(try! FakeServer.reply(to: request, fixture: "error-response.json"))
            }
            return true
        }
        let store = PrMonStore(socketPath: server.path, startBackend: { Issue.record("started") })
        store.start()
        defer { store.stop() }
        try await waitUntil { store.isConnected && !store.toasts.isEmpty }
        #expect(store.state?.repos.count == 3)
        // The fixture's backend reports a warning, then the pushed toast arrives.
        #expect(store.toasts.map(\.message) == ["Ignoring unreadable config: example",
                                                 "Merged acme/api#1 (squash)"])
        store.markSeen(repo: "acme/api", number: 1)
        #expect(store.state?.entry("acme/api").unseen == [5])
        try await waitUntil { store.toasts.contains { $0.severity == .error } }
        #expect(store.toasts.last?.message == "acme/api#99 is not an open PR")
    }

    @Test func startsTheBackendOnceWhenItIsNotRunning() async throws {
        let starts = RequestLog()
        let store = PrMonStore(
            socketPath: "/tmp/prmonkit-missing.sock",
            retryDelay: .milliseconds(20),
            startBackend: { starts.append("start"); throw Launcher.Failure(output: "pr-mon: not found") }
        )
        store.start()
        defer { store.stop() }
        try await waitUntil { if case .disconnected = store.phase { true } else { false } }
        #expect(store.phase == .disconnected("couldn't start the backend: pr-mon: not found"))
        try await Task.sleep(for: .milliseconds(100))
        #expect(starts.ops == ["start"])
        #expect(store.phase == .disconnected("The pr-mon backend isn't running"))
    }
}

@MainActor
func waitUntil(_ condition: () -> Bool) async throws {
    for _ in 0..<200 where !condition() {
        try await Task.sleep(for: .milliseconds(10))
    }
    #expect(condition())
}

final class RequestLog: @unchecked Sendable {
    private let lock = NSLock()
    private var entries: [String] = []

    func append(_ op: String) {
        lock.withLock { entries.append(op) }
    }

    var ops: [String] {
        lock.withLock { entries }
    }
}
