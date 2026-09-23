// Decode the messages the backend generates (protocol-fixtures/, `make fixtures`).

import Foundation
import Testing

@testable import PrMonKit

enum Fixtures {
    static let directory = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()  // PrMonKitTests
        .deletingLastPathComponent()  // Tests
        .deletingLastPathComponent()  // PrMonKit
        .deletingLastPathComponent()  // macos
        .deletingLastPathComponent()  // repo root
        .appendingPathComponent("protocol-fixtures")

    static func data(_ name: String) throws -> Data {
        try Data(contentsOf: directory.appendingPathComponent(name))
    }

    /// The fixture as one wire line (compact JSON plus newline).
    static func line(_ name: String) throws -> Data {
        let object = try JSONSerialization.jsonObject(with: data(name))
        var line = try JSONSerialization.data(withJSONObject: object)
        line.append(0x0A)
        return line
    }

    static func snapshot() throws -> Snapshot {
        try JSONDecoder().decode(ResultBox<Snapshot>.self, from: data("snapshot-response.json")).result
    }

    static func event(_ kind: String) throws -> BackendEvent {
        try Wire.decodeEvent(kind, from: data("event-\(kind).json"))
    }
}

@Suite struct FixtureTests {
    @Test func hello() throws {
        let hello = try JSONDecoder().decode(
            ResultBox<Hello>.self, from: Fixtures.data("hello-response.json")
        ).result
        #expect(hello.protocol == protocolVersion)
        #expect(hello.version == "0.0.0-fixture")
        #expect(hello.merging == false)
    }

    @Test func snapshot() throws {
        let snapshot = try Fixtures.snapshot()
        #expect(snapshot.config.repos == ["acme/api", "acme/web", "Textualize/rich"])
        #expect(snapshot.config.notifications["acme/api"]?.scriptEnabled == true)
        #expect(snapshot.config.pollInterval == 120 && snapshot.config.focusInterval == 60
                && snapshot.config.activeInterval == 10)
        #expect(snapshot.errors["acme/web"] == "Repository acme/web not found")
        #expect(snapshot.unseen["acme/api"] == [1, 5])
        #expect(snapshot.collapsed == ["textualize"])
        #expect(snapshot.armed["Textualize/rich"]?["42"]?.method == .squash)
        #expect(snapshot.status.lastUpdate == "2026-09-17T12:00:00+00:00")

        let api = try #require(snapshot.repos["acme/api"])
        #expect(api.prTotal == 60)
        #expect(api.mergeMethods == [.squash, .merge, .rebase])
        #expect(Set(api.prs.map(\.status))
            == [.ready, .pending, .failing, .conflict, .behind, .draft, .checking, .waiting])
        #expect(api.prs[0].title == "Ready to go 🚀")
        #expect(api.prs[5].title == "WIP\u{2028}with a line separator")

        let failing = api.prs[2]
        #expect(failing.reasons.first == Reason(text: "Check failed: lint", level: .error))
        #expect(failing.checks.count == 2)
        #expect(failing.lastCheckStartedAt == "2026-09-15T01:30:00Z")

        let autoMerged = api.prs[7]
        #expect(autoMerged.autoMerge == AutoMerge(method: .rebase, enabledBy: "carol"))
        #expect(autoMerged.headRefId == nil)
    }

    @Test func closingIssues() throws {
        let api = try #require(try Fixtures.snapshot().repos["acme/api"])
        let linked = api.prs[0].closingIssues
        #expect(linked.map(\.number) == [12, 7])
        #expect(linked.first?.title == "Crash on empty config")
        // An issue in this repo is "#12"; one elsewhere carries its repository.
        #expect(linked[0].label(in: "acme/api") == "#12")
        #expect(linked[1].label(in: "acme/api") == "acme/infra#7")
        #expect(api.prs[1].closingIssues.isEmpty)
    }

    @Test func dependencies() throws {
        let api = try #require(try Fixtures.snapshot().repos["acme/api"])
        let waiting = api.prs[8]
        #expect(waiting.status == .waiting)
        #expect(waiting.waitsOn.map(\.key) == ["acme/api#2", "acme/lib#5"])
        #expect(waiting.waitsOn[0].status == .pending && waiting.waitsOn[0].state == .open)
        #expect(waiting.waitsOn[1].state == .merged && waiting.waitsOn[1].status == nil)
        #expect(waiting.waitsOn[0].label(in: "acme/api") == "#2")
        #expect(waiting.waitsOn[1].label(in: "acme/api") == "acme/lib#5")
        #expect(waiting.actions[1].kind == "arm_merge")
        #expect(api.prs[1].requiredBy.map(\.key) == ["acme/api#9"])
        #expect(api.prs[0].waitsOn.isEmpty && api.prs[0].requiredBy.isEmpty)

        let graph = try JSONDecoder().decode(
            ResultBox<DependencyGraph>.self, from: Fixtures.data("dependency-graph-response.json")
        ).result
        #expect(graph.pr.key == "acme/api#2")
        #expect(graph.waitsOn.map(\.pr.state) == [.unknown])
        #expect(graph.waitsOn[0].branches == nil)
        #expect(graph.requiredBy.map(\.pr.key) == ["acme/api#9"])
        #expect(graph.requiredBy[0].repeated == false)
    }

    @Test func actions() throws {
        let snapshot = try Fixtures.snapshot()
        let api = try #require(snapshot.repos["acme/api"])
        let ready = api.prs[0].actions
        #expect(ready.map(\.key) == ["merge", "auto_merge", "draft"])
        #expect(ready[0].available && ready[0].needsMethod && ready[0].offersDeleteBranch)
        #expect(ready[1].reason == "already mergeable")
        #expect(api.prs[4].actions.map(\.kind) == ["merge", "auto_merge_on", "update", "convert_to_draft"])
        // A PR with a failed GitHub Actions run offers to re-run it.
        #expect(api.prs[2].actions.map(\.key) == ["merge", "auto_merge", "draft", "rerun"])
        #expect(api.prs[2].checks.first?.runId == 4242)
        #expect(api.prs[0].actions.contains { $0.key == "rerun" } == false)

        let rich = try #require(snapshot.repos["Textualize/rich"])
        #expect(rich.mergeMethods == [.squash])
        #expect(rich.prs.map { $0.actions[1].kind } == ["arm_merge", "disarm_merge"])
    }

    @Test(arguments: ["repos", "repo", "seen", "collapsed", "config", "status", "toast"])
    func events(kind: String) throws {
        let event = try Fixtures.event(kind)
        switch (kind, event) {
        case let ("repos", .repos(snapshot)): #expect(snapshot == (try Fixtures.snapshot()))
        case let ("repo", .repo(update)):
            #expect(update.name == "acme/api")
            #expect(update.repo?.prs.count == 9)
            #expect(update.unseen == [1, 5])
        case let ("seen", .seen(update)): #expect(update.unseen == [1, 5])
        case let ("collapsed", .collapsed(owners)): #expect(owners == ["textualize"])
        case let ("config", .config(config)): #expect(config.pollInterval == 120)
        case let ("status", .status(status)): #expect(status.pid == 4242)
        case let ("toast", .toast(toast)):
            #expect(toast == Toast(message: "Merged acme/api#1 (squash)", severity: .information))
        default: Issue.record("\(kind) decoded as \(event)")
        }
    }

    @Test func unknownEventIsIgnored() throws {
        #expect(try Wire.decodeEvent("future", from: Data("{}".utf8)) == .unknown("future"))
    }

    /// The backend's default notification settings, as sent in the fixture requests.
    private func defaults(script: String = "") -> NotifyConfig {
        NotifyConfig(
            message: "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}",
            events: ["READY", "FAILING", "CONFLICT", "MERGED", "MERGE_FAILED"],
            script: script
        )
    }

    @Test func notificationFormAndPreview() throws {
        let form = try JSONDecoder().decode(
            ResultBox<NotificationForm>.self, from: Fixtures.data("notification-form-response.json")
        ).result
        #expect(form.events.first == EventOption(name: "READY", label: "Ready (mergeable)"))
        #expect(form.events.count == 9)
        #expect(form.variables.contains("PR_REASON"))
        #expect(form.scriptHelp.contains("stdin"))
        #expect(form.defaults == defaults())
        let preview = try JSONDecoder().decode(
            ResultBox<NotificationPreview>.self,
            from: Fixtures.data("preview-notification-response.json")
        ).result
        #expect(preview == NotificationPreview(text: "acme/api#123 {{PR_OOPS}}", unknown: ["PR_OOPS"]))
    }

    @Test func requestsMatchTheBackend() throws {
        let expected = try JSONSerialization.jsonObject(with: Fixtures.data("requests.json"))
            as! [NSDictionary]
        let action = { (kind: String, method: MergeMethod?, delete: Bool) in
            PerformArgs(repo: "acme/api", number: 1,
                        action: Action(kind: kind, method: method, deleteBranch: delete))
        }
        let encoded: [Data] = [
            try Wire.encode(Request(id: 1, op: "hello", args: NoArgs())),
            try Wire.encode(Request(id: 2, op: "snapshot", args: NoArgs())),
            try Wire.encode(Request(id: 3, op: "mark_seen",
                                    args: RepoNumberArgs(name: "acme/api", number: 1))),
            try Wire.encode(Request(id: 4, op: "refresh_all", args: NoArgs())),
            try Wire.encode(Request(id: 5, op: "perform", args: action("merge", .squash, true))),
            try Wire.encode(Request(id: 6, op: "perform", args: action("arm_merge", .merge, false))),
            try Wire.encode(Request(id: 7, op: "perform", args: action("disarm_merge", nil, false))),
            try Wire.encode(Request(id: 8, op: "perform", args: action("update", nil, false))),
            try Wire.encode(Request(id: 22, op: "perform", args: action("mark_ready", nil, false))),
            try Wire.encode(Request(id: 23, op: "perform", args: action("rerun_checks", nil, false))),
            try Wire.encode(Request(id: 9, op: "add_repo", args: NameArgs(name: "acme/web"))),
            try Wire.encode(Request(id: 10, op: "remove_repo", args: NameArgs(name: "acme/web"))),
            try Wire.encode(Request(id: 11, op: "set_collapsed",
                                    args: CollapsedArgs(owner: "acme", collapsed: true))),
            try Wire.encode(Request(id: 21, op: "set_focus",
                                    args: FocusArgs(repo: "acme/api", number: 1))),
            try Wire.encode(Request(id: 12, op: "save_notifications",
                                    args: SettingsArgs(repo: "acme/api", settings: defaults(script: "im")))),
            try Wire.encode(Request(id: 13, op: "send_test",
                                    args: SettingsArgs(repo: "acme/api", settings: defaults()))),
            try Wire.encode(Request(id: 14, op: "notification_form", args: NoArgs())),
            try Wire.encode(Request(id: 15, op: "preview_notification",
                                    args: PreviewArgs(repo: "acme/api", message: "{{PR_NUM}}"))),
            try Wire.encode(Request(id: 16, op: "set_poll_interval", args: SecondsArgs(seconds: 120))),
            try Wire.encode(Request(id: 17, op: "add_dependency",
                                    args: DependencyArgs(repo: "acme/api", number: 9, on: "acme/api#2"))),
            try Wire.encode(Request(id: 18, op: "remove_dependency",
                                    args: DependencyArgs(repo: "acme/api", number: 9, on: "acme/lib#5"))),
            try Wire.encode(Request(id: 19, op: "dependency_graph",
                                    args: PRArgs(repo: "acme/api", number: 2))),
        ]
        #expect(encoded.count == expected.count)
        for (data, expectedRequest) in zip(encoded, expected) {
            #expect(data.last == 0x0A)
            #expect(!data.dropLast().contains(0x0A))
            let swift = try JSONSerialization.jsonObject(with: data) as! NSDictionary
            #expect(swift == expectedRequest)
        }
    }

    @Test func socketPaths() throws {
        struct Cases: Decodable {
            struct Case: Decodable {
                var state_directory: String
                var socket: String
            }
            var uid: UInt32
            var cases: [Case]
        }
        let fixture = try JSONDecoder().decode(Cases.self, from: Fixtures.data("socket-paths.json"))
        for item in fixture.cases {
            #expect(SocketPath.socket(stateDirectory: item.state_directory, uid: fixture.uid)
                == item.socket)
        }
    }

    @Test func configFile() {
        #expect(SocketPath.configFile(environment: [:], home: "/Users/a")
            == "/Users/a/.config/pr-mon/config.toml")
        #expect(SocketPath.configFile(environment: ["XDG_CONFIG_HOME": "/x/config"], home: "/Users/a")
            == "/x/config/pr-mon/config.toml")
        #expect(SocketPath.configFile(environment: ["XDG_CONFIG_HOME": ""], home: "/Users/a")
            == "/Users/a/.config/pr-mon/config.toml")
    }

    @Test func stateDirectory() {
        #expect(SocketPath.stateDirectory(environment: [:], home: "/Users/a")
            == "/Users/a/.local/state/pr-mon")
        #expect(SocketPath.stateDirectory(environment: ["XDG_STATE_HOME": "/x/state"], home: "/Users/a")
            == "/x/state/pr-mon")
        #expect(SocketPath.stateDirectory(environment: ["XDG_STATE_HOME": ""], home: "/Users/a")
            == "/Users/a/.local/state/pr-mon")
    }
}

@Suite struct DecodingTests {
    @Test func unknownValuesAndFieldsAreTolerated() throws {
        let json = #"{"text": "x", "level": "critical", "added_later": 1}"#
        let reason = try JSONDecoder().decode(Reason.self, from: Data(json.utf8))
        #expect(reason.level == .unknown)
        #expect(try JSONDecoder().decode(PRStatus.self, from: Data(#""MERGING""#.utf8)) == .unknown)
    }
}
