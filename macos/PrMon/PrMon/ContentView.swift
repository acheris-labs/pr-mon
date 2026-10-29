// The main window: repositories | pull requests | details, like Mail.

import AppKit
import PrMonKit
import SwiftUI

struct ContentView: View {
    let store: PrMonStore
    let selection: AppSelection
    @Environment(\.openSettings) private var openSettings
    @AppStorage("appearance") private var appearance = Appearance.system
    @State private var repoName: String?
    @State private var prID: PullRequest.ID?
    @State private var request: ActionRequest?
    @State private var waitingTarget: PRTarget?
    @State private var graphTarget: PRTarget?
    @State private var columns = NavigationSplitViewVisibility.all
    /// The list you last picked a row in: Command-1 to Command-9 pick from it.
    @State private var listFocus = ListFocus.prs
    @State private var query = ""
    /// A search result in another repo: selected once the sidebar has moved there.
    @State private var pendingPR: PullRequest.ID?

    private var state: BackendState? { store.state }

    private var entry: BackendState.RepoEntry? {
        guard let repoName, state?.snapshot.config.repos.contains(repoName) == true else { return nil }
        return state?.entry(repoName)
    }

    private var selectedPR: PullRequest? {
        entry?.repo?.prs.first { $0.id == prID }
    }

    /// Search results, or nil when the search field is empty.
    private var results: [BackendState.SearchResult]? {
        guard let state, !query.trimmingCharacters(in: .whitespaces).isEmpty else { return nil }
        return state.search(query)
    }

    /// The repo a PR belongs to, wherever it is listed.
    private func repo(of pr: PullRequest) -> Repo? {
        state?.repos.first { $0.repo?.prs.contains { $0.id == pr.id } == true }?.repo
    }

    var body: some View {
        Group {
            if let state {
                NavigationSplitView(columnVisibility: $columns) {
                    Sidebar(
                        store: store, state: state, selection: picked($repoName, in: .repos),
                        openRepoSettings: openRepoSettings
                    )
                        .navigationSplitViewColumnWidth(min: 200, ideal: 240, max: 360)
                } content: {
                    PRList(
                        entry: entry, results: results, query: query, state: state,
                        selection: results == nil ? picked($prID, in: .prs) : searchSelection,
                        act: { pr, option in act(on: pr, option) },
                        depend: { pr, command in depend(pr, command) }
                    )
                    .navigationSplitViewColumnWidth(min: 300, ideal: 420)
                } detail: {
                    PRDetailColumn(
                        entry: entry, pr: selectedPR, state: state,
                        depend: { pr, command in depend(pr, command) },
                        select: { number in prID = entry?.repo?.prs.first { $0.number == number }?.id },
                        stopWaiting: { pr, ref in
                            guard let repo = entry?.name else { return }
                            store.removeDependency(repo: repo, number: pr.number, on: ref.key)
                        }
                    )
                }
            } else {
                ConnectionUnavailable(store: store)
            }
        }
        .toolbar { toolbar }
        .overlay(alignment: .bottom) { ToastStack(toasts: store.toasts) }
        .sheet(item: $request) { request in
            ActionSheet(request: request) { action in
                store.perform(repo: request.repo.name, number: request.pr.number, action: action)
            }
        }
        .sheet(item: $waitingTarget) { DependencyPicker(store: store, target: $0) }
        .sheet(item: $graphTarget) { DependencyGraphSheet(store: store, target: $0) }
        .focusedSceneValue(\.pullRequest, focusedContext)
        .focusedSceneValue(\.listJump, listJump)
        .onChange(of: repoName) {
            selection.repo = repoName
            prID = pendingPR
            pendingPR = nil
            store.setFocus(repo: repoName, number: nil)
        }
        .onChange(of: prID) {
            if let repoName, let pr = selectedPR {
                store.markSeen(repo: repoName, number: pr.number)
                store.setFocus(repo: repoName, number: pr.number)
            }
        }
        .onChange(of: state?.snapshot.config.repos) { keepSelectionValid() }
        .onChange(of: appearance) { NSApp.appearance = appearance.nsAppearance }
        .onAppear {
            keepSelectionValid()
            store.setFocus(repo: repoName, number: nil)
            NSApp.appearance = appearance.nsAppearance
        }
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        // A search field of our own: .searchable always puts it after Refresh.
        // The macOS 26 SDK's toolbar API needs Xcode 26; older Xcodes build
        // without it.
        #if compiler(>=6.2)
        if #available(macOS 26, *) {
            // Drawn as its own capsule: the toolbar's glass would otherwise
            // wrap the field and Refresh together.
            ToolbarItem(placement: .primaryAction) {
                searchField
                    .padding(.horizontal, 6)
                    .frame(height: 34)
                    .background(Capsule().fill(.quaternary))
            }
            .sharedBackgroundVisibility(.hidden)
        } else {
            ToolbarItem(placement: .primaryAction) { searchField }
        }
        #else
        ToolbarItem(placement: .primaryAction) { searchField }
        #endif
        ToolbarItem(placement: .primaryAction) {
            Button { store.refreshAll() } label: {
                Label("Refresh", systemImage: "arrow.clockwise")
            }
            .help("Check GitHub now")
            .disabled(!store.isConnected)
        }
    }

    private var searchField: some View {
        SearchField(text: $query, prompt: "Title, author or #number")
            .frame(minWidth: 180, idealWidth: 260, maxWidth: 320)
            .disabled(state == nil)
    }

    private var focusedContext: PullRequestContext? {
        guard let repo = entry?.repo, let pr = selectedPR else { return nil }
        return PullRequestContext(
            repo: repo, pr: pr, act: { option in act(on: pr, option) },
            depend: { command in depend(pr, command) }
        )
    }

    /// A list's selection, noting that you picked from it. Command-1 to
    /// Command-9 set the selection directly, so they don't move it.
    private func picked<T>(_ selection: Binding<T>, in list: ListFocus) -> Binding<T> {
        Binding(get: { selection.wrappedValue }, set: {
            selection.wrappedValue = $0
            listFocus = list
        })
    }

    /// Picking a search result moves the sidebar to its repo, then selects it.
    private var searchSelection: Binding<PullRequest.ID?> {
        Binding(get: { prID }, set: { id in
            listFocus = .prs
            let owner = state?.repos.first { $0.repo?.prs.contains { $0.id == id } == true }?.name
            if let id, let owner, owner != repoName {
                pendingPR = id
                repoName = owner
            } else {
                prID = id
            }
        })
    }

    private var listJump: ListJump? {
        guard let state else { return nil }
        if listFocus == .repos {
            let repos = state.owners.filter { !$0.collapsed }.flatMap(\.repos).prefix(9)
            return ListJump(titles: repos.map(\.name)) { repoName = repos[repos.startIndex + $0].name }
        }
        let prs = (entry?.repo?.prs ?? []).prefix(9)
        return ListJump(titles: prs.map { "#\($0.number) \($0.title)" }) { prID = prs[prs.startIndex + $0].id }
    }

    private func depend(_ pr: PullRequest, _ command: DependencyCommand) {
        guard let repo = repo(of: pr)?.name else { return }
        let target = PRTarget(repo: repo, number: pr.number, title: pr.title)
        switch command {
        case .waitFor: waitingTarget = target
        case .showGraph: graphTarget = target
        }
    }

    /// Open Settings on one repo's notifications.
    private func openRepoSettings(_ name: String?) {
        if let name { selection.repo = name }
        selection.settingsTab = .repositories
        openSettings()
    }

    private func keepSelectionValid() {
        guard let repos = state?.owners.flatMap({ $0.repos.map(\.name) }) else { return }
        if repoName == nil || !repos.contains(repoName!) {
            repoName = repos.first
        }
    }

    /// Runs an option at once, or asks first when there is something to choose or confirm.
    private func act(on pr: PullRequest, _ option: ActionOption) {
        guard let repo = repo(of: pr), option.available else { return }
        let request = ActionRequest(repo: repo, pr: pr, option: option)
        if request.needsConfirmation {
            self.request = request
        } else {
            store.perform(repo: repo.name, number: pr.number, action: request.immediateAction)
        }
    }
}

/// An AppKit search field, so it can sit anywhere in the toolbar.
private struct SearchField: NSViewRepresentable {
    @Binding var text: String
    let prompt: String

    func makeNSView(context: Context) -> NSSearchField {
        let field = NSSearchField()
        field.placeholderString = prompt
        field.sendsSearchStringImmediately = true
        field.target = context.coordinator
        field.action = #selector(Coordinator.changed(_:))
        return field
    }

    func updateNSView(_ field: NSSearchField, context: Context) {
        context.coordinator.text = $text
        if field.stringValue != text { field.stringValue = text }
    }

    func makeCoordinator() -> Coordinator { Coordinator(text: $text) }

    @MainActor
    final class Coordinator: NSObject {
        var text: Binding<String>

        init(text: Binding<String>) { self.text = text }

        /// Each keystroke, and the clear button.
        @objc func changed(_ field: NSSearchField) {
            text.wrappedValue = field.stringValue
        }
    }
}

// MARK: - sidebar

private struct Sidebar: View {
    let store: PrMonStore
    let state: BackendState
    @Binding var selection: String?
    let openRepoSettings: (String?) -> Void

    var body: some View {
        List(selection: $selection) {
            ForEach(state.owners) { group in
                Section(isExpanded: expanded(group)) {
                    ForEach(group.repos) { entry in
                        RepoRow(entry: entry)
                            .tag(entry.name)
                            .contextMenu {
                                Button("Open on GitHub") {
                                    Browser.open("https://github.com/\(entry.name)/pulls")
                                }
                                Button("Notification Settings…") { openRepoSettings(entry.name) }
                            }
                    }
                } header: {
                    Text(group.owner)
                }
            }
        }
        .listStyle(.sidebar)
        .overlay {
            // Only when the backend has said so: with none connected, an empty
            // list means "nothing to show yet", not "you monitor nothing".
            if state.snapshot.config.repos.isEmpty {
                if store.isConnected {
                    ContentUnavailableView {
                        Label("No Repositories", systemImage: "tray")
                    } description: {
                        Text("Add repositories to monitor in Settings.")
                    } actions: {
                        Button("Open Settings…") { openRepoSettings(nil) }
                    }
                } else {
                    ContentUnavailableView {
                        Label(store.phase == .startingBackend
                              ? "Starting the Backend…" : "Not Connected",
                              systemImage: "bolt.horizontal.circle")
                    } description: {
                        Text("Your repositories are listed once the backend answers.")
                    }
                }
            }
        }
        .safeAreaInset(edge: .bottom) { ConnectionStatus(store: store) }
    }

    private func expanded(_ group: BackendState.OwnerGroup) -> Binding<Bool> {
        Binding(
            get: { !group.collapsed },
            set: { store.setCollapsed(owner: group.key, collapsed: !$0) }
        )
    }
}

private struct RepoRow: View {
    let entry: BackendState.RepoEntry

    var body: some View {
        let unseen = entry.unseenPRs.count
        Label {
            Text(entry.shortName)
        } icon: {
            if entry.error != nil {
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
            } else if entry.repo == nil {
                ProgressView().controlSize(.mini)
            } else {
                Image(systemName: "shippingbox")
            }
        }
        .badge(unseen)
        .help(entry.error ?? entry.name)
    }
}

private struct ConnectionStatus: View {
    let store: PrMonStore

    var body: some View {
        HStack(spacing: 6) {
            Circle()
                .fill(store.isConnected ? Color.green : Color.red)
                .frame(width: 7, height: 7)
            Text(text)
                .lineLimit(1)
                .truncationMode(.tail)
            Spacer()
        }
        .font(.caption)
        .foregroundStyle(.secondary)
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .help(help)
    }

    private var status: BackendStatus? { store.state?.snapshot.status }

    private var text: String {
        switch store.phase {
        case .connected:
            if let until = Format.date(status?.rateLimitedUntil) {
                return "Rate limited until \(until.formatted(date: .omitted, time: .shortened))"
            }
            if let updated = Format.date(status?.lastUpdate) {
                return "Updated \(updated.formatted(date: .omitted, time: .shortened))"
            }
            return "Connected"
        case .connecting: return "Connecting…"
        case .startingBackend: return "Starting backend…"
        case .disconnected: return "Disconnected — reconnecting"
        case .incompatible: return "Incompatible backend"
        }
    }

    private var help: String {
        switch store.phase {
        case let .disconnected(reason), let .incompatible(reason): reason
        default: status.map { "pr-mon backend \($0.version)" } ?? ""
        }
    }
}

private struct ConnectionUnavailable: View {
    let store: PrMonStore

    var body: some View {
        switch store.phase {
        case let .disconnected(reason):
            ContentUnavailableView {
                Label("Not Connected", systemImage: "bolt.horizontal.circle")
            } description: {
                Text("\(reason). pr-mon keeps retrying.")
            } actions: {
                Button("Start Backend") { Task { await store.launchBackend() } }
            }
        case let .incompatible(reason):
            ContentUnavailableView("Incompatible Backend", systemImage: "exclamationmark.triangle",
                                   description: Text(reason))
        case .startingBackend:
            ProgressView("Starting the pr-mon backend…")
        case .connecting, .connected:
            ProgressView("Connecting…")
        }
    }
}

// MARK: - pull request list

private struct PRList: View {
    let entry: BackendState.RepoEntry?
    /// Search results, shown instead of the repo's PRs while searching.
    let results: [BackendState.SearchResult]?
    let query: String
    let state: BackendState
    @Binding var selection: PullRequest.ID?
    let act: (PullRequest, ActionOption) -> Void
    let depend: (PullRequest, DependencyCommand) -> Void

    var body: some View {
        List(selection: $selection) {
            if let results {
                ForEach(results) { result in
                    Section(result.entry.name) {
                        ForEach(result.prs) { row($0, in: result.entry) }
                    }
                }
            } else if let entry {
                ForEach(entry.repo?.prs ?? []) { row($0, in: entry) }
            }
        }
        .contextMenu(forSelectionType: PullRequest.ID.self) { ids in
            if let id = ids.first, let (repo, pr) = find(id) {
                PRMenuItems(repo: repo, pr: pr, act: { act(pr, $0) }, depend: { depend(pr, $0) })
            }
        } primaryAction: { ids in
            if let id = ids.first, let (_, pr) = find(id) {
                Browser.open(pr.url)
            }
        }
        .overlay { placeholder }
        .navigationTitle(results == nil ? entry?.name ?? "pr-mon" : "Search")
        .navigationSubtitle(subtitle)
    }

    private func row(_ pr: PullRequest, in entry: BackendState.RepoEntry) -> some View {
        PRRow(
            pr: pr,
            unseen: entry.unseen.contains(pr.number),
            armed: state.armed(repo: entry.name, number: pr.number) != nil
        )
    }

    /// A listed PR and its repo.
    private func find(_ id: PullRequest.ID) -> (Repo, PullRequest)? {
        let entries = results?.map(\.entry) ?? (entry.map { [$0] } ?? [])
        for entry in entries {
            if let repo = entry.repo, let pr = repo.prs.first(where: { $0.id == id }) {
                return (repo, pr)
            }
        }
        return nil
    }

    @ViewBuilder
    private var placeholder: some View {
        if let results {
            if results.isEmpty {
                ContentUnavailableView.search(text: query)
            }
        } else if let entry {
            if let error = entry.error, entry.repo == nil {
                ContentUnavailableView("Couldn't Load", systemImage: "exclamationmark.triangle",
                                       description: Text(error))
            } else if entry.repo == nil {
                ProgressView()
            } else if entry.repo?.prs.isEmpty == true {
                ContentUnavailableView("No Open Pull Requests", systemImage: "checkmark.circle")
            }
        } else {
            ContentUnavailableView("No Repository Selected", systemImage: "sidebar.left")
        }
    }

    private var subtitle: String {
        if let results {
            let count = results.reduce(0) { $0 + $1.prs.count }
            return count == 1 ? "1 pull request" : "\(count) pull requests"
        }
        guard let repo = entry?.repo else { return "" }
        if repo.prTotal > repo.prs.count {
            return "Newest \(repo.prs.count) of \(repo.prTotal) open"
        }
        return "\(repo.prTotal) open"
    }
}

private struct PRRow: View {
    let pr: PullRequest
    let unseen: Bool
    let armed: Bool

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Circle()
                .fill(unseen ? Color.accentColor : Color.clear)
                .frame(width: 8, height: 8)
                .accessibilityLabel(unseen ? "New" : "")
            Image(systemName: pr.status.symbol)
                .foregroundStyle(pr.status.color)
                .help(pr.status.title)
            VStack(alignment: .leading, spacing: 2) {
                Text(pr.title)
                    .fontWeight(unseen ? .semibold : .regular)
                    .lineLimit(2)
                HStack(spacing: 6) {
                    Text(verbatim: "#\(pr.number)")
                    Text(pr.author)
                    if pr.muted == true {
                        Image(systemName: "bell.slash")
                            .help("Notifications muted")
                            .accessibilityLabel("Notifications muted")
                    }
                }
                .font(.subheadline)
                .foregroundStyle(.secondary)
                HStack(spacing: 4) {
                    ForEach(pr.status.chips, id: \.text) { chip in
                        Chip(text: chip.text, tint: chip.tint, symbol: chip.symbol)
                    }
                    if pr.autoMerge != nil {
                        Chip(text: "Auto-merge", tint: .teal, symbol: "arrow.triangle.merge")
                            .help("GitHub auto-merge is on")
                    } else if armed {
                        Chip(text: "Merge when ready", tint: .accentColor, symbol: "arrow.triangle.merge")
                            .help("pr-mon merges it once it's ready")
                    }
                    if pr.autoRetry == true {
                        Chip(text: "Auto-retry", tint: .purple, symbol: "arrow.clockwise")
                            .help("pr-mon re-runs failed checks once, when their run finishes")
                    }
                }
            }
        }
        .padding(.vertical, 3)
    }
}

/// A short tag under a PR's title, tinted by what it says: its status, or a
/// merge waiting to happen (teal for GitHub's auto-merge, the accent colour
/// for pr-mon's).
private struct Chip: View {
    let text: String
    let tint: Color
    let symbol: String

    var body: some View {
        Label(text, systemImage: symbol)
            .font(.caption.weight(.semibold))
            .foregroundStyle(tint)
            .padding(.vertical, 1)
            .padding(.horizontal, 6)
            .background(tint.opacity(0.15), in: Capsule())
            .fixedSize()
    }
}

// MARK: - detail

private struct PRDetailColumn: View {
    let entry: BackendState.RepoEntry?
    let pr: PullRequest?
    let state: BackendState
    let depend: (PullRequest, DependencyCommand) -> Void
    let select: (Int) -> Void
    let stopWaiting: (PullRequest, PRRef) -> Void

    var body: some View {
        if let pr, let repo = entry?.repo {
            PRDetail(
                repo: repo, pr: pr, armed: state.armed(repo: repo.name, number: pr.number),
                depend: { depend(pr, $0) }, stopWaiting: { stopWaiting(pr, $0) }
            )
                .toolbar {
                    // PR actions (merge, auto-merge, draft, re-run…) live in the
                    // right-click menu and the Pull Request menu; the toolbar
                    // keeps what applies whatever state the PR is in.
                    ToolbarItemGroup {
                        Menu {
                            Button("Wait for Another Pull Request…") { depend(pr, .waitFor) }
                            Button("Show Dependency Graph…") { depend(pr, .showGraph) }
                        } label: {
                            Label("Dependencies", systemImage: "point.3.connected.trianglepath.dotted")
                        }
                        .help("What this pull request waits on")
                        Button { Browser.open(pr.url) } label: {
                            Label("Open in Browser", systemImage: "safari")
                        }
                        .help("Open on GitHub")
                    }
                }
        } else if let repo = entry?.repo, !repo.prs.isEmpty {
            RepoGraphView(repo: repo, select: select)
        } else {
            ContentUnavailableView("No Pull Request Selected", systemImage: "arrow.triangle.pull")
        }
    }
}

private struct PRDetail: View {
    let repo: Repo
    let pr: PullRequest
    let armed: ArmedMerge?
    let depend: (DependencyCommand) -> Void
    let stopWaiting: (PRRef) -> Void

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                header
                if !pr.reasons.isEmpty {
                    GroupBox(pr.status == .ready ? "Notes" : "Blocked By") {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(Array(pr.reasons.enumerated()), id: \.offset) { _, reason in
                                Label {
                                    Text(reason.text)
                                } icon: {
                                    Image(systemName: reason.level.symbol).foregroundStyle(reason.level.color)
                                }
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(4)
                    }
                }
                if !pr.waitsOn.isEmpty {
                    GroupBox {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(pr.waitsOn) { ref in
                                HStack {
                                    PRRefRow(ref: ref, repo: repo.name)
                                    Spacer()
                                    Button { stopWaiting(ref) } label: {
                                        Image(systemName: "minus.circle")
                                    }
                                    .buttonStyle(.borderless)
                                    .help("Stop waiting on \(ref.label(in: repo.name))")
                                }
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(4)
                    } label: {
                        dependencyHeading("Waits On")
                    }
                }
                if !pr.requiredBy.isEmpty {
                    GroupBox {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(pr.requiredBy) { PRRefRow(ref: $0, repo: repo.name) }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(4)
                    } label: {
                        dependencyHeading("Required By")
                    }
                }
                if !pr.closingIssues.isEmpty {
                    GroupBox("Closes") {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(pr.closingIssues) { issue in
                                issueLine(issue)
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(4)
                    }
                }
                GroupBox("Details") {
                    Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 6) {
                        row("Branch", Text(verbatim: "\(pr.headRef) → \(pr.baseRef)"))
                        row("Author", Text(pr.author))
                        dateRow("Opened", pr.createdAt)
                        dateRow("Last commit", pr.lastCommitAt)
                        dateRow("Last check", pr.lastCheckStartedAt)
                        if let sha = pr.headSha {
                            row("Head", Text(sha).font(.body.monospaced()))
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(4)
                }
            }
            .padding(20)
            .frame(maxWidth: .infinity, alignment: .leading)
            .textSelection(.enabled)
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(pr.title).font(.title2).bold()
            Label(pr.status.title, systemImage: pr.status.symbol)
                .foregroundStyle(pr.status.color)
                .fontWeight(.medium)
            // A merge waiting to happen is the thing you most want to spot, so it
            // gets its own line and a colour of its own rather than more grey text.
            if let auto = pr.autoMerge {
                chip("Auto-merge on (\(Format.method(auto.method)), by \(auto.enabledBy))",
                     tint: .teal)
            } else if let armed {
                chip("pr-mon merges when ready (\(Format.method(armed.method))"
                     + (armed.deleteBranch ? ", deletes branch)" : ")"),
                     tint: .accentColor)
            }
            if let url = URL(string: pr.url) {
                Link(destination: url) {
                    Text(verbatim: "\(repo.name)#\(pr.number)")
                }
            }
        }
    }

    private func dependencyHeading(_ title: String) -> some View {
        HStack {
            Text(title)
            Spacer()
            Button("Add…") { depend(.waitFor) }
            Button("Show Graph…") { depend(.showGraph) }
        }
        .buttonStyle(.link)
        .font(.callout)
    }

    private func chip(_ text: String, tint: Color) -> some View {
        Label(text, systemImage: "arrow.triangle.merge")
            .font(.callout.weight(.medium))
            .foregroundStyle(tint)
            .padding(.vertical, 4)
            .padding(.horizontal, 10)
            .background(tint.opacity(0.15), in: Capsule())
    }

    private func row(_ label: String, _ value: Text) -> some View {
        GridRow {
            Text(label).foregroundStyle(.secondary).gridColumnAlignment(.trailing)
            value
        }
    }

    @ViewBuilder
    private func issueLine(_ issue: LinkedIssue) -> some View {
        Label {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                if let url = URL(string: issue.url) {
                    Link(issue.label(in: repo.name), destination: url)
                } else {
                    Text(issue.label(in: repo.name))
                }
                Text(issue.title)
            }
        } icon: {
            Image(systemName: "smallcircle.filled.circle").foregroundStyle(.secondary)
        }
    }

    @ViewBuilder
    private func dateRow(_ label: String, _ iso: String?) -> some View {
        if let date = Format.date(iso) {
            GridRow {
                Text(label).foregroundStyle(.secondary).gridColumnAlignment(.trailing)
                Timestamp(date: date)
            }
        }
    }
}

// MARK: - toasts

private struct ToastStack: View {
    let toasts: [PrMonStore.ToastItem]

    var body: some View {
        VStack(spacing: 6) {
            ForEach(toasts) { toast in
                Label {
                    Text(toast.message).fixedSize(horizontal: false, vertical: true)
                } icon: {
                    Image(systemName: toast.severity.symbol).foregroundStyle(toast.severity.color)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 8)
                .frame(maxWidth: 460)
                .background(.regularMaterial, in: Capsule())
                .shadow(radius: 4, y: 2)
                .transition(.move(edge: .bottom).combined(with: .opacity))
            }
        }
        .padding(.bottom, 16)
        .animation(.easeOut(duration: 0.2), value: toasts)
        .allowsHitTesting(false)
    }
}
