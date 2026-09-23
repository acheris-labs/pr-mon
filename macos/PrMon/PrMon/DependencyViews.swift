// PR dependencies: choosing a PR to wait on, and the graph around a PR. The
// backend keeps the graph and holds merges back; these only show and edit it.

import PrMonKit
import SwiftUI

/// The PR a dependency sheet is about.
struct PRTarget: Identifiable, Equatable {
    var repo: String
    var number: Int
    var title: String
    var id: String { "\(repo)#\(number)" }
}

/// One PR a dependency names: where it stands, its number and its title.
struct PRRefRow: View {
    let ref: PRRef
    /// The repo it is shown from, so its own PRs need no owner/repo prefix.
    let repo: String
    var note: String? = nil

    var body: some View {
        Label {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                if let url = URL(string: ref.url) {
                    Link(ref.label(in: repo), destination: url)
                } else {
                    Text(ref.label(in: repo))
                }
                Text(ref.title.isEmpty ? " " : ref.title)
                    .lineLimit(1)
                    .truncationMode(.tail)
                Text(note ?? ref.stateTitle)
                    .foregroundStyle(.secondary)
            }
        } icon: {
            Image(systemName: ref.symbol).foregroundStyle(ref.color)
        }
        .help(ref.stateTitle)
    }
}

/// Picks a PR for the target to wait on: any open PR in a monitored repo, or
/// anything typed as owner/repo#12 or a pull request URL.
struct DependencyPicker: View {
    let store: PrMonStore
    let target: PRTarget
    @Environment(\.dismiss) private var dismiss
    @State private var search = ""
    @State private var chosen: Set<PRRef.ID> = []
    @State private var problem: String?
    @State private var adding = false

    private var waitsOn: Set<String> {
        let pr = store.state?.snapshot.repos[target.repo]?.prs.first { $0.number == target.number }
        return Set(pr?.waitsOn.map(\.key) ?? [])
    }

    private var candidates: [PRRef] {
        let words = search.lowercased().split(separator: " ").map(String.init)
        let taken = waitsOn.union([target.id])
        let repos = (store.state?.snapshot.repos ?? [:]).values.sorted { $0.name < $1.name }
        return repos.flatMap { repo in
            repo.prs.compactMap { pr -> PRRef? in
                let ref = PRRef(repo: repo.name, number: pr.number, title: pr.title, url: pr.url,
                                state: .open, status: pr.status)
                let text = "\(ref.key) \(pr.title) \(pr.author)".lowercased()
                guard !taken.contains(ref.key), words.allSatisfy(text.contains) else { return nil }
                return ref
            }
        }
    }

    /// What Add sends: everything picked (⌘-click or shift-click for several),
    /// the only match, or the text as typed.
    private var choices: [String] {
        let matches = candidates
        let picked = matches.filter { chosen.contains($0.id) }.map(\.key)
        if !picked.isEmpty {
            return picked
        }
        if matches.count == 1 {
            return [matches[0].key]
        }
        let typed = search.trimmingCharacters(in: .whitespaces)
        return matches.isEmpty && !typed.isEmpty ? [typed] : []
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text(verbatim: "Wait for another pull request before merging \(target.id)")
                    .font(.headline)
                Text(target.title)
                    .foregroundStyle(.secondary)
            }
            TextField("Search, owner/repo#12, or a pull request URL", text: $search)
                .textFieldStyle(.roundedBorder)
                .onSubmit(add)
            List(candidates, selection: $chosen) { ref in
                PRRefRow(ref: ref, repo: target.repo)
            }
            .frame(minHeight: 240)
            .overlay {
                if candidates.isEmpty {
                    ContentUnavailableView {
                        Label("No Matching Pull Requests", systemImage: "magnifyingglass")
                    } description: {
                        Text("Add uses what you typed, for a PR in a repository pr-mon doesn't monitor.")
                    }
                }
            }
            if let problem {
                Label(problem, systemImage: "xmark.octagon.fill")
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Text("Pick several with ⌘-click or shift-click. pr-mon won't merge \(target.id) "
                 + "until every pull request it waits on has merged.")
                .font(.callout)
                .foregroundStyle(.secondary)
            HStack {
                if adding { ProgressView().controlSize(.small) }
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button(addTitle, action: add)
                    .keyboardShortcut(.defaultAction)
                    .disabled(choices.isEmpty || adding)
            }
        }
        .padding(20)
        .frame(width: 560, height: 480)
        .onChange(of: search) { problem = nil }
    }

    private var addTitle: String {
        choices.count > 1 ? "Add \(choices.count)" : "Add"
    }

    private func add() {
        let wanted = choices
        guard !wanted.isEmpty, !adding else { return }
        adding = true
        problem = nil
        Task {
            var refused: [String] = []
            for on in wanted {
                do {
                    try await store.addDependency(repo: target.repo, number: target.number, on: on)
                } catch {
                    refused.append("\(on): \(error.localizedDescription)")
                }
            }
            adding = false
            if refused.isEmpty {
                dismiss()
                return
            }
            // The ones that worked are in; say which didn't and why.
            chosen = []
            problem = refused.joined(separator: "\n")
        }
    }
}

/// Everything connected to a PR: what it waits on above, what waits on it below.
struct DependencyGraphSheet: View {
    let store: PrMonStore
    let target: PRTarget
    @Environment(\.dismiss) private var dismiss
    @State private var graph: DependencyGraph?
    @State private var problem: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(verbatim: "Dependencies of \(target.id)")
                .font(.headline)
            Group {
                if let graph {
                    List {
                        Section("Waits On") { tree(graph.waitsOn) }
                        Section("This Pull Request") {
                            PRRefRow(ref: graph.pr, repo: target.repo)
                        }
                        Section("Required By") { tree(graph.requiredBy) }
                    }
                } else if let problem {
                    ContentUnavailableView("Couldn't Load the Graph",
                                           systemImage: "exclamationmark.triangle",
                                           description: Text(problem))
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            .frame(minHeight: 300)
            HStack {
                Spacer()
                Button("Done") { dismiss() }
                    .keyboardShortcut(.defaultAction)
            }
        }
        .padding(20)
        .frame(width: 600, height: 480)
        // Follow the backend: a merge or an edit elsewhere changes the graph.
        .task(id: store.state?.snapshot.repos[target.repo]) { await load() }
    }

    @ViewBuilder
    private func tree(_ nodes: [DependencyNode]) -> some View {
        if nodes.isEmpty {
            Text("Nothing").foregroundStyle(.secondary)
        } else {
            ForEach(nodes) { DependencyNodeRow(node: $0, repo: target.repo) }
        }
    }

    private func load() async {
        do {
            graph = try await store.dependencyGraph(repo: target.repo, number: target.number)
            problem = nil
        } catch {
            problem = error.localizedDescription
        }
    }
}

/// A node and, open by default, the PRs beyond it.
private struct DependencyNodeRow: View {
    let node: DependencyNode
    let repo: String
    @State private var expanded = true

    var body: some View {
        if node.children.isEmpty {
            row
        } else {
            DisclosureGroup(isExpanded: $expanded) {
                ForEach(node.children) { DependencyNodeRow(node: $0, repo: repo) }
            } label: {
                row
            }
        }
    }

    private var row: some View {
        PRRefRow(ref: node.pr, repo: repo,
                 note: node.repeated ? "\(node.pr.stateTitle), shown above" : nil)
    }
}
