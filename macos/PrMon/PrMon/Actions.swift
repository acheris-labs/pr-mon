// PR actions: the backend decides what's offered; these views let the user pick and confirm.

import PrMonKit
import SwiftUI

/// An action the user picked, waiting for a merge method, branch choice, or confirmation.
struct ActionRequest: Identifiable {
    var repo: Repo
    var pr: PullRequest
    var option: ActionOption
    var id: String { "\(repo.name)#\(pr.number):\(option.kind)" }

    var methods: [MergeMethod] {
        option.needsMethod ? repo.mergeMethods.filter { $0 != .unknown } : []
    }

    /// Merges always ask; other actions ask only when there is something to choose.
    var needsConfirmation: Bool {
        option.kind == "merge" || methods.count > 1 || option.offersDeleteBranch
    }

    /// The action to send when nothing needs asking.
    var immediateAction: Action {
        Action(kind: option.kind, method: methods.first)
    }

    /// Button title, with an ellipsis when choosing it asks for more.
    var buttonTitle: String {
        option.available && needsConfirmation ? "\(option.label)…" : option.label
    }

    /// Menu title; unavailable entries say why.
    var menuTitle: String {
        option.available ? buttonTitle : "\(option.label) (\(option.reason ?? "unavailable"))"
    }
}

struct ActionSheet: View {
    let request: ActionRequest
    let perform: (Action) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var method: MergeMethod
    @State private var deleteBranch = true

    init(request: ActionRequest, perform: @escaping (Action) -> Void) {
        self.request = request
        self.perform = perform
        _method = State(initialValue: request.methods.first ?? .squash)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            VStack(alignment: .leading, spacing: 4) {
                Text(verbatim: "\(request.option.label) \(request.repo.name)#\(request.pr.number)?")
                    .font(.headline)
                Text(request.pr.title)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if request.methods.count > 1 || request.option.offersDeleteBranch {
                Form {
                    if request.methods.count > 1 {
                        Picker("Method:", selection: $method) {
                            ForEach(request.methods, id: \.self) { Text($0.title).tag($0) }
                        }
                        .pickerStyle(.radioGroup)
                    }
                    if request.option.offersDeleteBranch {
                        Toggle(isOn: $deleteBranch) {
                            Text(verbatim: "Delete branch \(request.pr.headRef)")
                        }
                    }
                }
                .formStyle(.columns)
            }
            if let note = request.option.note {
                Label(note, systemImage: "info.circle")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button(request.option.label) {
                    perform(Action(
                        kind: request.option.kind,
                        method: request.option.needsMethod ? method : nil,
                        deleteBranch: request.option.offersDeleteBranch && deleteBranch
                    ))
                    dismiss()
                }
                .keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 440)
    }
}

/// What a PR's menus can ask for beyond its actions: its dependencies.
enum DependencyCommand {
    case waitFor, showGraph
}

/// Menu items for a PR, shared by the context menu and the menu bar.
struct PRMenuItems: View {
    let repo: Repo
    let pr: PullRequest
    let act: (ActionOption) -> Void
    let depend: (DependencyCommand) -> Void

    var body: some View {
        Button("Open in Browser") { Browser.open(pr.url) }
        Button("Copy Link") { Browser.copy(pr.url) }
        Divider()
        ForEach(pr.actions) { option in
            Button(ActionRequest(repo: repo, pr: pr, option: option).menuTitle) { act(option) }
                .disabled(!option.available)
        }
        Divider()
        Button("Wait for Another Pull Request…") { depend(.waitFor) }
        Button("Show Dependency Graph…") { depend(.showGraph) }
    }
}

// MARK: - menu bar

struct PullRequestContext {
    var repo: Repo
    var pr: PullRequest
    var act: (ActionOption) -> Void
    var depend: (DependencyCommand) -> Void
}

extension FocusedValues {
    @Entry var pullRequest: PullRequestContext?
}

struct PullRequestCommands: Commands {
    @FocusedValue(\.pullRequest) private var context

    var body: some Commands {
        CommandMenu("Pull Request") {
            if let context {
                Button("Open in Browser") { Browser.open(context.pr.url) }
                    .keyboardShortcut("o")
                Button("Copy Link") { Browser.copy(context.pr.url) }
                    .keyboardShortcut("c", modifiers: [.command, .shift])
                Divider()
                ForEach(context.pr.actions) { option in
                    Button(ActionRequest(repo: context.repo, pr: context.pr, option: option).menuTitle) {
                        context.act(option)
                    }
                        .disabled(!option.available)
                        .keyboardShortcut(Self.shortcut(option.key), modifiers: [.command, .option])
                }
                Divider()
                Button("Wait for Another Pull Request…") { context.depend(.waitFor) }
                    .keyboardShortcut("w", modifiers: [.command, .option])
                Button("Show Dependency Graph…") { context.depend(.showGraph) }
                    .keyboardShortcut("g", modifiers: [.command, .option])
            } else {
                Button("Open in Browser") {}.disabled(true)
                    .keyboardShortcut("o")
            }
        }
    }

    private static func shortcut(_ key: String) -> KeyEquivalent {
        switch key {
        case "merge": "m"
        case "auto_merge": "a"
        case "draft": "t"
        case "rerun": "r"
        default: "u"
        }
    }
}
