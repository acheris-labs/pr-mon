// A blocking Unix stream socket; reads run on a dedicated thread.

import Darwin
import Foundation

final class UnixSocket: @unchecked Sendable {
    private let fd: Int32
    private let writeLock = NSLock()

    private init(fd: Int32) {
        self.fd = fd
    }

    /// Connect, or throw `ClientError.unavailable` when nothing is listening.
    static func connect(path: String) throws -> UnixSocket {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw ClientError.io(String(cString: strerror(errno))) }
        var noSigPipe: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))

        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        let bytes = Array(path.utf8)
        guard bytes.count < capacity else {
            close(fd)
            throw ClientError.io("socket path too long: \(path)")
        }
        withUnsafeMutableBytes(of: &address.sun_path) { buffer in
            buffer.copyBytes(from: bytes)
            buffer[bytes.count] = 0
        }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0 else {
            let code = errno
            close(fd)
            if code == ENOENT || code == ECONNREFUSED {
                throw ClientError.unavailable
            }
            throw ClientError.io(String(cString: strerror(code)))
        }
        return UnixSocket(fd: fd)
    }

    func write(_ data: Data) throws {
        writeLock.lock()
        defer { writeLock.unlock() }
        try data.withUnsafeBytes { (buffer: UnsafeRawBufferPointer) in
            var offset = 0
            while offset < buffer.count {
                let sent = send(fd, buffer.baseAddress! + offset, buffer.count - offset, 0)
                if sent < 0 {
                    if errno == EINTR { continue }
                    throw ClientError.disconnected
                }
                offset += sent
            }
        }
    }

    /// Lines as they arrive; the stream ends when the backend closes the connection.
    func lines() -> AsyncStream<Data> {
        AsyncStream(bufferingPolicy: .unbounded) { continuation in
            let thread = Thread { [self] in
                var splitter = LineSplitter()
                var buffer = [UInt8](repeating: 0, count: 64 * 1024)
                while true {
                    let count = buffer.withUnsafeMutableBytes { read(fd, $0.baseAddress, $0.count) }
                    if count < 0, errno == EINTR { continue }
                    if count <= 0 { break }
                    for line in splitter.append(Data(buffer[0..<count])) {
                        continuation.yield(line)
                    }
                }
                continuation.finish()
            }
            thread.name = "pr-mon socket reader"
            thread.start()
        }
    }

    /// Unblocks the reader; the descriptor is released when the object goes away.
    func shutdown() {
        Darwin.shutdown(fd, SHUT_RDWR)
    }

    deinit {
        close(fd)
    }
}
