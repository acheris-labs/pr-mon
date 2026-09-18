// The client's mirror of the backend state, kept current by events.

import Foundation

public struct BackendState: Sendable, Equatable {
    public var snapshot: Snapshot

    public init(snapshot: Snapshot) {
        self.snapshot = snapshot
    }

    public mutating func apply(_ event: BackendEvent) {
        switch event {
        case let .repos(new):
            snapshot = new
        case let .repo(update):
            snapshot.repos[update.name] = update.repo
            snapshot.errors[update.name] = update.error
            snapshot.unseen[update.name] = update.unseen
            snapshot.armed[update.name] = update.armed
        case let .seen(update):
            snapshot.unseen[update.name] = update.unseen
        case let .collapsed(owners):
            snapshot.collapsed = owners
        case let .config(config):
            snapshot.config = config
        case let .status(status):
            snapshot.status = status
        case .toast, .unknown:
            break
        }
    }

    // MARK: local changes, applied before the backend confirms them

    public mutating func markSeenLocally(repo: String, number: Int) {
        snapshot.unseen[repo]?.removeAll { $0 == number }
    }

    public mutating func setCollapsedLocally(owner: String, collapsed: Bool) {
        var owners = Set(snapshot.collapsed)
        if collapsed { owners.insert(owner) } else { owners.remove(owner) }
        snapshot.collapsed = owners.sorted()
    }

    // MARK: display helpers (grouping only; the backend decided every value)

    public struct RepoEntry: Sendable, Equatable, Identifiable {
        public var name: String
        public var repo: Repo?
        public var error: String?
        public var unseen: Set<Int>
        public var id: String { name }

        /// The part after the owner.
        public var shortName: String {
            String(name.split(separator: "/", maxSplits: 1).last ?? Substring(name))
        }

        public var unseenPRs: [PullRequest] {
            repo?.prs.filter { unseen.contains($0.number) } ?? []
        }

        public func count(_ status: PRStatus) -> Int {
            repo?.prs.filter { $0.status == status }.count ?? 0
        }

        public var alerts: Int {
            repo?.prs.filter { $0.status.isAlert }.count ?? 0
        }
    }

    /// Repos under one owner, as the tree shows them.
    public struct OwnerGroup: Sendable, Equatable, Identifiable {
        /// Lower-cased owner: the grouping and collapse key.
        public var key: String
        /// The owner as spelled in the first repo.
        public var owner: String
        public var repos: [RepoEntry]
        public var collapsed: Bool
        public var id: String { key }

        public var unseenPRs: [PullRequest] { repos.flatMap(\.unseenPRs) }
        public var hasError: Bool { repos.contains { $0.error != nil } }
    }

    /// The colour of an unseen-PR badge.
    public enum Badge: Sendable {
        case ready, alert, other

        public init?(_ prs: [PullRequest]) {
            guard !prs.isEmpty else { return nil }
            if prs.contains(where: { $0.status == .ready }) {
                self = .ready
            } else if prs.contains(where: { $0.status.isAlert }) {
                self = .alert
            } else {
                self = .other
            }
        }
    }

    public static func ownerKey(_ repo: String) -> String {
        String(repo.split(separator: "/", maxSplits: 1).first ?? Substring(repo)).lowercased()
    }

    /// Configured repos in config order.
    public var repos: [RepoEntry] {
        snapshot.config.repos.map(entry)
    }

    /// Unseen PRs across every repo: the sidebar's badges added up, for the
    /// Dock. Only PRs still open count, as in the sidebar.
    public var unseenTotal: Int {
        repos.reduce(0) { $0 + $1.unseenPRs.count }
    }

    public func entry(_ name: String) -> RepoEntry {
        RepoEntry(
            name: name,
            repo: snapshot.repos[name],
            error: snapshot.errors[name],
            unseen: Set(snapshot.unseen[name] ?? [])
        )
    }

    /// Owner groups sorted by owner, repos sorted by name (case-insensitive).
    public var owners: [OwnerGroup] {
        let sorted = snapshot.config.repos.sorted {
            (Self.ownerKey($0), $0.lowercased()) < (Self.ownerKey($1), $1.lowercased())
        }
        var groups: [OwnerGroup] = []
        let collapsed = Set(snapshot.collapsed)
        for name in sorted {
            let key = Self.ownerKey(name)
            if groups.last?.key != key {
                let owner = String(name.split(separator: "/", maxSplits: 1).first ?? "")
                groups.append(OwnerGroup(
                    key: key, owner: owner, repos: [], collapsed: collapsed.contains(key)))
            }
            groups[groups.count - 1].repos.append(entry(name))
        }
        return groups
    }

    public func armed(repo: String, number: Int) -> ArmedMerge? {
        snapshot.armed[repo]?[String(number)]
    }
}
