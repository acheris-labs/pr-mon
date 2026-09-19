// Wire objects from docs/protocol.md. The backend decides everything; these only carry it.
// Unknown JSON fields are ignored; unknown enum values decode as `.unknown`.

import Foundation

/// A string enum that tolerates values added by newer backends.
public protocol LenientEnum: RawRepresentable, Codable, Sendable, Hashable
where RawValue == String {
    static var unknownValue: Self { get }
}

extension LenientEnum {
    public init(from decoder: Decoder) throws {
        let raw = try String(from: decoder)
        self = Self(rawValue: raw) ?? Self.unknownValue
    }
}

public enum PRStatus: String, LenientEnum {
    case draft = "DRAFT"
    case checking = "CHECKING"
    case conflict = "CONFLICT"
    case failing = "FAILING"
    case pending = "PENDING"
    case behind = "BEHIND"
    case blocked = "BLOCKED"
    /// Mergeable, but waiting on other PRs to merge first.
    case waiting = "WAITING"
    case ready = "READY"
    case unknown = "UNKNOWN"
    public static let unknownValue = PRStatus.unknown

    /// Statuses that need the user's attention.
    public var isAlert: Bool { [.conflict, .failing, .blocked].contains(self) }
}

public enum MergeMethod: String, LenientEnum, CaseIterable {
    case squash = "SQUASH"
    case merge = "MERGE"
    case rebase = "REBASE"
    case unknown = "UNKNOWN"
    public static let unknownValue = MergeMethod.unknown

    public var title: String {
        switch self {
        case .squash: "Squash and merge"
        case .merge: "Create a merge commit"
        case .rebase: "Rebase and merge"
        case .unknown: "Unknown method"
        }
    }
}

public enum ReasonLevel: String, LenientEnum {
    case error, warning, info, unknown
    public static let unknownValue = ReasonLevel.unknown
}

public enum ToastSeverity: String, LenientEnum {
    case information, warning, error, unknown
    public static let unknownValue = ToastSeverity.unknown
}

public struct Hello: Codable, Sendable, Equatable {
    public var `protocol`: Int?
    public var version: String
    public var pid: Int?
    public var notifier: String?
    public var merging: Bool
}

public struct BackendStatus: Codable, Sendable, Equatable {
    public var connected: Bool
    public var pid: Int?
    public var version: String
    public var notifier: String?
    public var lastUpdate: String?
    public var rateLimitedUntil: String?
    public var warnings: [String]

    enum CodingKeys: String, CodingKey {
        case connected, pid, version, notifier, warnings
        case lastUpdate = "last_update"
        case rateLimitedUntil = "rate_limited_until"
    }
}

public struct NotifyConfig: Codable, Sendable, Equatable {
    public var message: String
    public var events: [String]
    public var includeDrafts: Bool
    public var scriptEnabled: Bool
    public var script: String
    public var desktopEnabled: Bool

    public init(
        message: String, events: [String], includeDrafts: Bool = false,
        scriptEnabled: Bool = false, script: String = "", desktopEnabled: Bool = false
    ) {
        self.message = message
        self.events = events
        self.includeDrafts = includeDrafts
        self.scriptEnabled = scriptEnabled
        self.script = script
        self.desktopEnabled = desktopEnabled
    }

    enum CodingKeys: String, CodingKey {
        case message, events, script
        case includeDrafts = "include_drafts"
        case scriptEnabled = "script_enabled"
        case desktopEnabled = "desktop_enabled"
    }
}

public struct Config: Codable, Sendable, Equatable {
    public var repos: [String]
    public var pollInterval: Int
    public var notifications: [String: NotifyConfig]

    enum CodingKeys: String, CodingKey {
        case repos, notifications
        case pollInterval = "poll_interval"
    }
}

public struct Check: Codable, Sendable, Equatable {
    public var name: String
    public var state: String
    public var startedAt: String?

    enum CodingKeys: String, CodingKey {
        case name, state
        case startedAt = "started_at"
    }
}

public struct AutoMerge: Codable, Sendable, Equatable {
    public var method: MergeMethod
    public var enabledBy: String

    enum CodingKeys: String, CodingKey {
        case method
        case enabledBy = "enabled_by"
    }
}

public struct Reason: Codable, Sendable, Equatable {
    public var text: String
    public var level: ReasonLevel
}

public struct ArmedMerge: Codable, Sendable, Equatable {
    public var method: MergeMethod
    public var deleteBranch: Bool
    public var armedAt: String

    enum CodingKeys: String, CodingKey {
        case method
        case deleteBranch = "delete_branch"
        case armedAt = "armed_at"
    }
}

/// One entry of a PR's action menu, as the backend decided it.
public struct ActionOption: Codable, Sendable, Equatable, Identifiable {
    public var key: String
    public var kind: String
    public var label: String
    public var available: Bool
    public var reason: String?
    public var note: String?
    public var needsMethod: Bool
    public var offersDeleteBranch: Bool

    public var id: String { key }

    enum CodingKeys: String, CodingKey {
        case key, kind, label, available, reason, note
        case needsMethod = "needs_method"
        case offersDeleteBranch = "offers_delete_branch"
    }
}

/// An issue this PR closes when it merges. `repo` is the issue's own
/// repository, which is not always the one the PR is in.
public struct LinkedIssue: Codable, Sendable, Equatable, Identifiable {
    public var number: Int
    public var title: String
    public var url: String
    public var repo: String

    public var id: String { "\(repo)#\(number)" }

    /// "#12", or "other/repo#12" for an issue outside `repo`.
    public func label(in repo: String) -> String {
        self.repo.isEmpty || self.repo == repo ? "#\(number)" : "\(self.repo)#\(number)"
    }
}

/// Where a PR a dependency names stands on GitHub.
public enum PRState: String, LenientEnum {
    case open = "OPEN"
    case merged = "MERGED"
    case closed = "CLOSED"
    /// Not looked up yet.
    case unknown = "UNKNOWN"
    public static let unknownValue = PRState.unknown
}

/// A pull request anywhere on GitHub, named by a dependency. It may be in a
/// repo pr-mon doesn't monitor.
public struct PRRef: Codable, Sendable, Equatable, Hashable, Identifiable {
    public var repo: String
    public var number: Int
    public var title: String
    public var url: String
    public var state: PRState
    /// pr-mon's status, for an open PR in a monitored repo.
    public var status: PRStatus?

    public init(repo: String, number: Int, title: String, url: String, state: PRState,
                status: PRStatus? = nil) {
        self.repo = repo
        self.number = number
        self.title = title
        self.url = url
        self.state = state
        self.status = status
    }

    /// "owner/repo#12": how the backend names it in `add_dependency` and friends.
    public var key: String { "\(repo)#\(number)" }
    public var id: String { key }

    /// "#12", or "other/repo#12" for a PR outside `repo`.
    public func label(in repo: String) -> String {
        self.repo == repo ? "#\(number)" : key
    }
}

/// One PR in a dependency tree, with the next step in the same direction.
public struct DependencyNode: Codable, Sendable, Equatable, Identifiable {
    public var pr: PRRef
    public var children: [DependencyNode]
    /// Shown in full elsewhere in the tree, so `children` is empty.
    public var repeated: Bool

    public var id: String { pr.key }
    /// For outline views: nil for a leaf.
    public var branches: [DependencyNode]? { children.isEmpty ? nil : children }
}

/// Everything connected to one PR: what it waits on, and what waits on it.
public struct DependencyGraph: Codable, Sendable, Equatable {
    public var pr: PRRef
    public var waitsOn: [DependencyNode]
    public var requiredBy: [DependencyNode]

    enum CodingKeys: String, CodingKey {
        case pr
        case waitsOn = "waits_on"
        case requiredBy = "required_by"
    }
}

public struct PullRequest: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var number: Int
    public var title: String
    public var url: String
    public var author: String
    public var createdAt: String
    public var isDraft: Bool
    public var headRef: String
    public var baseRef: String
    public var headRefId: String?
    public var headRepo: String?
    public var mergeable: String
    public var mergeState: String
    public var reviewDecision: String?
    public var checkState: String?
    public var checks: [Check]
    public var checksTotal: Int
    public var autoMerge: AutoMerge?
    public var closingIssues: [LinkedIssue]
    public var lastCommitAt: String?
    public var headSha: String?
    public var status: PRStatus
    public var reasons: [Reason]
    public var strictlyReady: Bool
    public var lastCheckStartedAt: String?
    public var actions: [ActionOption]
    /// PRs that must merge before this one, and PRs waiting for this one.
    public var waitsOn: [PRRef]
    public var requiredBy: [PRRef]

    /// "owner/repo#12" of this PR.
    public func key(in repo: String) -> String { "\(repo)#\(number)" }

    enum CodingKeys: String, CodingKey {
        case id, number, title, url, author, mergeable, checks, status, reasons, actions
        case waitsOn = "waits_on"
        case requiredBy = "required_by"
        case createdAt = "created_at"
        case isDraft = "is_draft"
        case headRef = "head_ref"
        case baseRef = "base_ref"
        case headRefId = "head_ref_id"
        case headRepo = "head_repo"
        case mergeState = "merge_state"
        case reviewDecision = "review_decision"
        case checkState = "check_state"
        case checksTotal = "checks_total"
        case autoMerge = "auto_merge"
        case closingIssues = "closing_issues"
        case lastCommitAt = "last_commit_at"
        case headSha = "head_sha"
        case strictlyReady = "strictly_ready"
        case lastCheckStartedAt = "last_check_started_at"
    }
}

public struct Repo: Codable, Sendable, Equatable {
    public var name: String
    public var mergeMethods: [MergeMethod]
    public var deleteBranchOnMerge: Bool
    public var prs: [PullRequest]
    public var prTotal: Int
    public var autoMergeAllowed: Bool

    enum CodingKeys: String, CodingKey {
        case name, prs
        case mergeMethods = "merge_methods"
        case deleteBranchOnMerge = "delete_branch_on_merge"
        case prTotal = "pr_total"
        case autoMergeAllowed = "auto_merge_allowed"
    }
}

public struct EventOption: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var label: String
    public var id: String { name }
}

/// What a notification settings editor offers (from the backend).
public struct NotificationForm: Codable, Sendable, Equatable {
    public var events: [EventOption]
    public var variables: [String]
    public var scriptHelp: String
    /// Settings to start from for a repo that has none.
    public var defaults: NotifyConfig

    enum CodingKeys: String, CodingKey {
        case events, variables, defaults
        case scriptHelp = "script_help"
    }
}

public struct NotificationPreview: Codable, Sendable, Equatable {
    public var text: String
    public var unknown: [String]
}

/// PR numbers are JSON object keys, so they arrive as strings.
public typealias ArmedByNumber = [String: ArmedMerge]

public struct Snapshot: Codable, Sendable, Equatable {
    public var config: Config
    public var repos: [String: Repo]
    public var errors: [String: String]
    public var unseen: [String: [Int]]
    public var collapsed: [String]
    public var armed: [String: ArmedByNumber]
    public var status: BackendStatus
}

public struct RepoUpdate: Codable, Sendable, Equatable {
    public var name: String
    public var repo: Repo?
    public var error: String?
    public var unseen: [Int]
    public var armed: ArmedByNumber
}

public struct SeenUpdate: Codable, Sendable, Equatable {
    public var name: String
    public var unseen: [Int]
}

public struct Toast: Codable, Sendable, Equatable {
    public var message: String
    public var severity: ToastSeverity
}

/// A command for `perform`.
public struct Action: Encodable, Sendable, Equatable {
    public var kind: String
    public var method: MergeMethod?
    public var deleteBranch: Bool

    public init(kind: String, method: MergeMethod? = nil, deleteBranch: Bool = false) {
        self.kind = kind
        self.method = method
        self.deleteBranch = deleteBranch
    }

    enum CodingKeys: String, CodingKey {
        case kind, method
        case deleteBranch = "delete_branch"
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(kind, forKey: .kind)
        // Always present, null when there is no method.
        try container.encode(method, forKey: .method)
        try container.encode(deleteBranch, forKey: .deleteBranch)
    }
}
