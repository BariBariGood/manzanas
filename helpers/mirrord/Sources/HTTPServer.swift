import Foundation

/// Minimal HTTP/1.1 server over a unix socket. Requests are served
/// serially on one thread — deliberate: the mirroring window is one
/// shared physical resource, so serialization is a feature, not a
/// limitation.
final class HTTPServer {
    typealias Handler = (_ method: String, _ path: String, _ body: Data) -> (Int, [String: Any])

    private let socketPath: String
    private let handler: Handler
    private var fd: Int32 = -1

    init(socketPath: String, handler: @escaping Handler) {
        self.socketPath = socketPath
        self.handler = handler
    }

    func run() throws {
        // A peer that vanishes mid-response (client timeout/cancel) must
        // produce an EPIPE write error, not kill the process.
        signal(SIGPIPE, SIG_IGN)
        let dir = (socketPath as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(
            atPath: dir, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        chmod(dir, 0o700) // owner-only even when the directory pre-exists
        unlink(socketPath)
        // The socket is the only access control (no in-protocol auth):
        // make it owner-only from birth, not after a chmod race.
        let oldMask = umask(0o177)
        defer { umask(oldMask) }
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw posixError("socket") }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = socketPath.utf8CString
        guard pathBytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else {
            throw posixError("socket path too long")
        }
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            pathBytes.withUnsafeBytes { src in
                raw.copyMemory(from: UnsafeRawBufferPointer(rebasing: src.prefix(raw.count)))
            }
        }
        let size = socklen_t(MemoryLayout<sockaddr_un>.size)
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, size) }
        }
        guard bound == 0 else { throw posixError("bind \(socketPath)") }
        chmod(socketPath, 0o600) // manzanasd runs as the same user; nobody else gets input access
        guard listen(fd, 8) == 0 else { throw posixError("listen") }

        FileHandle.standardError.write("mirrord listening on \(socketPath)\n".data(using: .utf8)!)
        while true {
            let client = accept(fd, nil, nil)
            if client < 0 {
                // EINTR/ECONNABORTED are per-connection blips; anything
                // else (EMFILE, dead listener) would busy-loop — exit and
                // let launchd's KeepAlive restart us.
                if errno == EINTR || errno == ECONNABORTED { continue }
                FileHandle.standardError.write(
                    "mirrord: accept failed (errno \(errno)), exiting for relaunch\n".data(using: .utf8)!)
                exit(1)
            }
            // The server is single-threaded: a peer that stalls (partial
            // header, undelivered body) must not wedge the only worker.
            var tv = timeval(tv_sec: 10, tv_usec: 0)
            setsockopt(client, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
            setsockopt(client, SOL_SOCKET, SO_SNDTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
            serve(client: client)
            close(client)
        }
    }

    private func serve(client: Int32) {
        guard let (method, path, body) = readRequest(client: client) else { return }
        let (status, payload) = handler(method, path, body)
        let data = (try? JSONSerialization.data(withJSONObject: payload)) ?? Data("{}".utf8)
        var resp = "HTTP/1.1 \(status) \(status / 100 == 2 ? "OK" : "Error")\r\n"
        resp += "Content-Type: application/json\r\n"
        resp += "Content-Length: \(data.count)\r\n"
        resp += "Connection: close\r\n\r\n"
        var out = Data(resp.utf8)
        out.append(data)
        out.withUnsafeBytes { raw in
            var sent = 0
            while sent < raw.count {
                let n = write(client, raw.baseAddress!.advanced(by: sent), raw.count - sent)
                if n <= 0 { break }
                sent += n
            }
        }
    }

    private func readRequest(client: Int32) -> (String, String, Data)? {
        var buf = Data()
        var chunk = [UInt8](repeating: 0, count: 65536)
        var headerEnd: Range<Data.Index>?
        // Read until the header terminator.
        while headerEnd == nil {
            let n = read(client, &chunk, chunk.count)
            if n <= 0 { return nil }
            buf.append(contentsOf: chunk[0..<n])
            headerEnd = buf.range(of: Data("\r\n\r\n".utf8))
            if buf.count > 1 << 20 { return nil }
        }
        guard let he = headerEnd,
              let head = String(data: buf[..<he.lowerBound], encoding: .utf8) else { return nil }
        let lines = head.components(separatedBy: "\r\n")
        let parts = lines[0].split(separator: " ")
        guard parts.count >= 2 else { return nil }
        var contentLength = 0
        for line in lines.dropFirst() {
            let kv = line.split(separator: ":", maxSplits: 1)
            if kv.count == 2, kv[0].trimmingCharacters(in: .whitespaces).lowercased() == "content-length" {
                contentLength = Int(kv[1].trimmingCharacters(in: .whitespaces)) ?? 0
            }
        }
        // Bodies are small JSON action payloads; a huge declared length
        // would just balloon the single worker's memory. Same 1 MiB cap
        // as the header.
        if contentLength < 0 || contentLength > 1 << 20 { return nil }
        var body = Data(buf[he.upperBound...])
        while body.count < contentLength {
            let n = read(client, &chunk, chunk.count)
            if n <= 0 { break }
            body.append(contentsOf: chunk[0..<n])
        }
        return (String(parts[0]), String(parts[1]), body)
    }

    private func posixError(_ what: String) -> NSError {
        NSError(domain: NSPOSIXErrorDomain, code: Int(errno),
                userInfo: [NSLocalizedDescriptionKey: "\(what): \(String(cString: strerror(errno)))"])
    }
}
