// Shared presentation helpers: status symbols, time formatting, links.

import AppKit
import PrMonKit
import SwiftUI

extension PRStatus {
    var title: String {
        switch self {
        case .ready: "Ready"
        case .failing: "Checks Failing"
        case .conflict: "Merge Conflict"
        case .blocked: "Blocked"
        case .behind: "Behind Base"
        case .pending: "Checks Running"
        case .draft: "Draft"
        case .waiting: "Waiting on Other PRs"
        case .checking: "Checking Mergeability"
        case .unknown: "Unknown"
        }
    }

    var symbol: String {
        switch self {
        case .ready: "checkmark.circle.fill"
        case .failing: "xmark.circle.fill"
        case .conflict: "exclamationmark.triangle.fill"
        case .blocked: "minus.circle.fill"
        case .behind: "arrow.down.circle.fill"
        case .pending: "clock.fill"
        case .draft: "pencil.circle"
        case .waiting: "hourglass.circle.fill"
        case .checking, .unknown: "questionmark.circle"
        }
    }

    var color: Color {
        switch self {
        case .ready: .green
        case .failing, .conflict: .red
        case .blocked, .behind: .orange
        case .waiting: .cyan
        case .pending, .draft, .checking, .unknown: .secondary
        }
    }
}

extension PRRef {
    /// Merged or closed first: that is what matters about a PR something waits on.
    var stateTitle: String {
        switch state {
        case .merged: "Merged"
        case .closed: "Closed without merging"
        case .open: status?.title ?? "Open"
        case .unknown: "Not looked up yet"
        }
    }

    var symbol: String {
        switch state {
        case .merged: "checkmark.circle.fill"
        case .closed: "xmark.circle.fill"
        case .open: status?.symbol ?? "circle"
        case .unknown: "questionmark.circle"
        }
    }

    var color: Color {
        switch state {
        case .merged: .purple
        case .closed: .red
        case .open: status?.color ?? .secondary
        case .unknown: .secondary
        }
    }
}

extension ReasonLevel {
    var symbol: String {
        switch self {
        case .error: "xmark.octagon.fill"
        case .warning: "exclamationmark.triangle.fill"
        case .info, .unknown: "info.circle"
        }
    }

    var color: Color {
        switch self {
        case .error: .red
        case .warning: .orange
        case .info, .unknown: .secondary
        }
    }
}

extension ToastSeverity {
    var symbol: String {
        switch self {
        case .error: "xmark.octagon.fill"
        case .warning: "exclamationmark.triangle.fill"
        case .information, .unknown: "checkmark.circle.fill"
        }
    }

    var color: Color {
        switch self {
        case .error: .red
        case .warning: .orange
        case .information, .unknown: .secondary
        }
    }
}

enum Format {
    // Only used from the main thread (SwiftUI views).
    nonisolated(unsafe) private static let parsers: [ISO8601DateFormatter] = {
        let plain = ISO8601DateFormatter()
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions.insert(.withFractionalSeconds)
        return [plain, fractional]
    }()

    static func date(_ iso: String?) -> Date? {
        guard let iso else { return nil }
        return parsers.lazy.compactMap { $0.date(from: iso) }.first
    }

    static func method(_ method: MergeMethod) -> String {
        switch method {
        case .squash: "squash"
        case .merge: "merge commit"
        case .rebase: "rebase"
        case .unknown: "unknown method"
        }
    }
}

/// "Sep 16, 9:10 AM (2 hours ago)"
struct Timestamp: View {
    let date: Date

    var body: some View {
        TimelineView(.periodic(from: .now, by: 60)) { context in
            Text("\(date.formatted(date: .abbreviated, time: .shortened)) (\(date.formatted(.relative(presentation: .named, unitsStyle: .wide))))")
                .id(context.date)
        }
    }
}

enum Browser {
    /// Opens web links only.
    static func open(_ link: String) {
        guard let url = URL(string: link), url.scheme == "https" else { return }
        NSWorkspace.shared.open(url)
    }

    static func copy(_ link: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(link, forType: .string)
    }
}
