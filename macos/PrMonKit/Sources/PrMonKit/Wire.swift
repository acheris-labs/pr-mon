// Framing and envelopes: one JSON object per line (docs/protocol.md, "Framing").

import Foundation

/// The protocol version this client implements.
public let protocolVersion = 1

/// Splits a byte stream into lines on "\n" only. (Foundation's line readers also
/// split on U+2028, which may appear raw inside JSON strings.)
public struct LineSplitter: Sendable {
    private var buffer = Data()

    public init() {}

    public mutating func append(_ data: Data) -> [Data] {
        buffer.append(data)
        var lines: [Data] = []
        while let newline = buffer.firstIndex(of: 0x0A) {
            let line = buffer[buffer.startIndex..<newline]
            if !line.isEmpty {
                lines.append(Data(line))
            }
            buffer.removeSubrange(buffer.startIndex...newline)
        }
        return lines
    }
}

/// The parts of any incoming message needed to route it.
struct Envelope: Decodable {
    var id: Int?
    var ok: Bool?
    var error: String?
    var event: String?
}

struct ResultBox<T: Decodable>: Decodable {
    var result: T
}

struct DataBox<T: Decodable>: Decodable {
    var data: T
}

struct Request<Args: Encodable>: Encodable {
    var id: Int
    var op: String
    var args: Args
}

public struct NoArgs: Encodable, Sendable {
    public init() {}
}

public struct RepoNumberArgs: Encodable, Sendable {
    var name: String
    var number: Int
}

struct NameArgs: Encodable, Sendable {
    var name: String
}

struct CollapsedArgs: Encodable, Sendable {
    var owner: String
    var collapsed: Bool
}

struct SettingsArgs: Encodable, Sendable {
    var repo: String
    var settings: NotifyConfig
}

struct SecondsArgs: Encodable, Sendable {
    var seconds: Int
}

struct PreviewArgs: Encodable, Sendable {
    var repo: String
    var message: String
}

public struct PerformArgs: Encodable, Sendable {
    var repo: String
    var number: Int
    var action: Action
}

/// A pushed backend event.
public enum BackendEvent: Sendable, Equatable {
    case repos(Snapshot)
    case repo(RepoUpdate)
    case seen(SeenUpdate)
    case collapsed([String])
    case config(Config)
    case status(BackendStatus)
    case toast(Toast)
    case unknown(String)
}

private struct CollapsedData: Decodable { var collapsed: [String] }
private struct ConfigData: Decodable { var config: Config }
private struct StatusData: Decodable { var status: BackendStatus }

enum Wire {
    static func encode(_ request: Request<some Encodable>) throws -> Data {
        var data = try JSONEncoder().encode(request)
        data.append(0x0A)
        return data
    }

    static func decodeEvent(_ name: String, from line: Data) throws -> BackendEvent {
        let decoder = JSONDecoder()
        switch name {
        case "repos": return .repos(try decoder.decode(DataBox<Snapshot>.self, from: line).data)
        case "repo": return .repo(try decoder.decode(DataBox<RepoUpdate>.self, from: line).data)
        case "seen": return .seen(try decoder.decode(DataBox<SeenUpdate>.self, from: line).data)
        case "collapsed":
            return .collapsed(try decoder.decode(DataBox<CollapsedData>.self, from: line).data.collapsed)
        case "config":
            return .config(try decoder.decode(DataBox<ConfigData>.self, from: line).data.config)
        case "status":
            return .status(try decoder.decode(DataBox<StatusData>.self, from: line).data.status)
        case "toast": return .toast(try decoder.decode(DataBox<Toast>.self, from: line).data)
        default: return .unknown(name)
        }
    }
}
