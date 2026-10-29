// Settings (⌘,): appearance and startup, repositories and their notifications,
// whose PRs are whose, backend control.

import PrMonKit
import ServiceManagement
import SwiftUI

/// UI state shared by the main window and Settings.
@MainActor
@Observable
final class AppSelection {
    /// The repo selected in the main window; Settings opens on it.
    var repo: String?
    /// The tab Settings opens on.
    var settingsTab: SettingsTab = .general
}

enum SettingsTab: String {
    case general, repositories, authors, backend
}

enum Appearance: String, CaseIterable, Identifiable {
    case system, light, dark
    var id: String { rawValue }

    var title: String {
        switch self {
        case .system: "System"
        case .light: "Light"
        case .dark: "Dark"
        }
    }

    var nsAppearance: NSAppearance? {
        switch self {
        case .system: nil
        case .light: NSAppearance(named: .aqua)
        case .dark: NSAppearance(named: .darkAqua)
        }
    }
}

struct SettingsView: View {
    let store: PrMonStore
    let selection: AppSelection

    var body: some View {
        TabView(selection: Bindable(selection).settingsTab) {
            GeneralSettings(store: store)
                .tabItem { Label("General", systemImage: "gearshape") }
                .tag(SettingsTab.general)
            RepositorySettings(store: store, selection: selection)
                .tabItem { Label("Repositories", systemImage: "shippingbox") }
                .tag(SettingsTab.repositories)
            AuthorSettings(store: store)
                .tabItem { Label("Authors", systemImage: "person.2") }
                .tag(SettingsTab.authors)
            BackendSettings(store: store)
                .tabItem { Label("Backend", systemImage: "bolt.horizontal") }
                .tag(SettingsTab.backend)
        }
        .frame(width: 800, height: 560)
    }
}

// MARK: - general

struct GeneralSettings: View {
    let store: PrMonStore
    @AppStorage("appearance") private var appearance = Appearance.system
    @AppStorage("hideEmptyRepos") private var hideEmptyRepos = false
    @State private var openAtLogin = SMAppService.mainApp.status == .enabled
    @State private var backendAtLogin = Launcher.AutostartState.disabled
    @State private var error: String?

    private static let intervals = [30, 60, 120, 300, 600, 1800]

    private var pollInterval: Int { store.state?.snapshot.config.pollInterval ?? 60 }

    var body: some View {
        Form {
            Section {
                Picker("Appearance:", selection: $appearance) {
                    ForEach(Appearance.allCases) { Text($0.title).tag($0) }
                }
                .pickerStyle(.segmented)
            }

            Section {
                Toggle("Hide repositories with no open pull requests", isOn: $hideEmptyRepos)
            } header: {
                Text("Sidebar")
            } footer: {
                Text("A repository still loading, one that couldn't load, and the one you have selected stay listed.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            Section {
                Picker("Check GitHub:", selection: Binding(
                    get: { pollInterval },
                    set: { store.setPollInterval($0) }
                )) {
                    ForEach(intervals, id: \.self) { Text("Every \(label(for: $0))").tag($0) }
                    if !Self.intervals.contains(pollInterval) {
                        Text("Every \(label(for: pollInterval))").tag(pollInterval)
                    }
                }
                .disabled(!store.isConnected)
            } footer: {
                Text("The backend polls this often; it also refreshes right after you act on a PR.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            Section {
                Toggle("Open pr-mon at login", isOn: Binding(
                    get: { openAtLogin },
                    set: { setOpenAtLogin($0) }
                ))
                Toggle("Start the backend at login", isOn: Binding(
                    get: { backendAtLogin == .enabled || backendAtLogin == .needsApproval },
                    set: { setBackendAtLogin($0) }
                ))
                .disabled(backendAtLogin == .unavailable)
                if let error {
                    Text(error).foregroundStyle(.red).font(.callout)
                }
            } header: {
                Text("Startup")
            } footer: {
                VStack(alignment: .leading, spacing: 4) {
                    Text("On: the backend starts at login and keeps checking GitHub, notifying and "
                        + "merging all the time, listed as pr-mon in Login Items. Off: it runs while "
                        + "PrMon or a dashboard is open, and stops two minutes after the last one quits.")
                    if backendAtLogin == .needsApproval {
                        Label("Allow pr-mon in System Settings \u{203A} General \u{203A} Login Items.",
                              systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                    }
                    if backendAtLogin == .unavailable {
                        Label("Install the pr-mon command line (`make install`) to change this.",
                              systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .task { backendAtLogin = await Launcher.autostartState() }
    }

    private var intervals: [Int] { Self.intervals }

    private func label(for seconds: Int) -> String {
        Duration.seconds(seconds).formatted(.units(allowed: [.minutes, .seconds], width: .wide))
    }

    private func setOpenAtLogin(_ on: Bool) {
        do {
            if on {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            openAtLogin = SMAppService.mainApp.status == .enabled
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func setBackendAtLogin(_ on: Bool) {
        let previous = backendAtLogin
        backendAtLogin = on ? .enabled : .disabled
        Task {
            do {
                try await Launcher.setAutostart(on)
                error = nil
            } catch {
                self.error = error.localizedDescription
                backendAtLogin = previous
            }
            backendAtLogin = await Launcher.autostartState()
        }
    }
}

// MARK: - backend

struct BackendSettings: View {
    let store: PrMonStore
    @State private var busy = false
    @State private var message: String?
    @State private var confirmingStop = false
    @State private var commandAvailable = true

    private var status: BackendStatus? { store.state?.snapshot.status }

    var body: some View {
        Form {
            Section {
                LabeledContent("Status", value: statusText)
                if let status {
                    LabeledContent("Version", value: status.version)
                    if let pid = status.pid {
                        LabeledContent("Process", value: String(pid))
                    }
                    LabeledContent("Desktop notifier", value: status.notifier ?? "none found")
                }
                LabeledContent("Socket") {
                    Text(store.socketPath).textSelection(.enabled).foregroundStyle(.secondary)
                }
                LabeledContent("Settings file") {
                    HStack {
                        Text(configPath)
                            .textSelection(.enabled)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                            .help(configPath)
                        Button("Show in Finder") {
                            NSWorkspace.shared.activateFileViewerSelecting(
                                [URL(fileURLWithPath: configPath)])
                        }
                    }
                }
                LabeledContent("Log") {
                    HStack {
                        Text(logPath).textSelection(.enabled).foregroundStyle(.secondary)
                        Button("Show in Finder") {
                            NSWorkspace.shared.activateFileViewerSelecting(
                                [URL(fileURLWithPath: logPath)])
                        }
                    }
                }
            }

            if let warnings = status?.warnings, !warnings.isEmpty {
                Section("Warnings") {
                    ForEach(warnings, id: \.self) { warning in
                        Label(warning, systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                    }
                }
            }

            Section {
                HStack {
                    Button("Refresh Now") { store.refreshAll() }
                        .disabled(!store.isConnected || busy)
                    Button("Restart Backend") { control("restarted", Launcher.restartBackend) }
                        .disabled(busy || !commandAvailable)
                    Button("Stop Backend") { confirmingStop = true }
                        .disabled(!store.isConnected || busy || !commandAvailable)
                    if busy { ProgressView().controlSize(.small) }
                }
                if let message {
                    Text(message).font(.callout).foregroundStyle(.secondary)
                }
            } footer: {
                if commandAvailable {
                    Text("Restart and stop run the pr-mon command line.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    Label("The pr-mon command line isn't installed (`make tool-install`).",
                          systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
        }
        .formStyle(.grouped)
        .task { commandAvailable = await Launcher.commandAvailable() }
        .confirmationDialog("Stop the pr-mon backend?", isPresented: $confirmingStop) {
            Button("Stop Backend", role: .destructive) { control("stopped", Launcher.stopBackend) }
        } message: {
            Text("Polling, notifications and merge-when-ready stop until it runs again.")
        }
    }

    private var configPath: String { SocketPath.configFile() }

    private var logPath: String {
        (SocketPath.stateDirectory() as NSString).appendingPathComponent("daemon.log")
    }

    private var statusText: String {
        switch store.phase {
        case .connected: "Connected"
        case .connecting: "Connecting…"
        case .startingBackend: "Starting…"
        case let .disconnected(reason): reason
        case let .incompatible(reason): reason
        }
    }

    private func control(_ done: String, _ action: @escaping () async throws -> Void) {
        busy = true
        message = nil
        Task {
            do {
                try await action()
                message = "Backend \(done)."
                store.retryNow()
            } catch {
                message = error.localizedDescription
            }
            busy = false
        }
    }
}

// MARK: - repositories

struct RepositorySettings: View {
    let store: PrMonStore
    let selection: AppSelection
    @State private var selected: String?
    @State private var tab = RepoSettingsTab.delivery
    @State private var adding = false
    @State private var confirmingRemoval = false

    private var repos: [String] {
        (store.state?.snapshot.config.repos ?? []).sorted { $0.lowercased() < $1.lowercased() }
    }

    var body: some View {
        HStack(spacing: 0) {
            repoList
                .frame(width: 230)
            Divider()
            detail
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .onAppear { selected = selection.repo ?? repos.first }
        .onChange(of: repos) {
            if let current = selected, !repos.contains(current) { selected = repos.first }
        }
        .sheet(isPresented: $adding) {
            AddRepoSheet(add: store.addRepo) { added in
                selected = added
            }
        }
        .confirmationDialog(
            "Stop monitoring \(selected ?? "")?",
            isPresented: $confirmingRemoval
        ) {
            Button("Stop Monitoring", role: .destructive) { remove() }
        } message: {
            Text("Its notification settings are removed too.")
        }
    }

    private var repoList: some View {
        VStack(spacing: 0) {
            // Grouped by owner, like the main window's sidebar.
            List(selection: $selected) {
                ForEach(store.state?.owners ?? []) { group in
                    Section(group.owner) {
                        ForEach(group.repos) { entry in
                            RepoListRow(entry: entry)
                                .tag(entry.name)
                                .listRowSeparator(.hidden)
                        }
                    }
                }
            }
            .listStyle(.sidebar)
            .onDeleteCommand { if selected != nil { confirmingRemoval = true } }
            Divider()
            HStack(spacing: 0) {
                Button { adding = true } label: {
                    Image(systemName: "plus").frame(width: 24, height: 20)
                }
                .help("Add a repository")
                Divider().frame(height: 16)
                Button { confirmingRemoval = true } label: {
                    Image(systemName: "minus").frame(width: 24, height: 20)
                }
                .help("Stop monitoring the selected repository")
                .disabled(selected == nil)
                Spacer()
            }
            .buttonStyle(.borderless)
            .padding(4)
            .disabled(!store.isConnected)
        }
    }

    @ViewBuilder
    private var detail: some View {
        if !store.isConnected {
            ContentUnavailableView(
                "Not Connected", systemImage: "bolt.horizontal.circle",
                description: Text("Settings are stored by the pr-mon backend.")
            )
        } else if let selected {
            NotificationSettings(store: store, repo: selected, tab: $tab)
                .id(selected)
        } else {
            ContentUnavailableView(
                "No Repositories", systemImage: "tray",
                description: Text("Click + to start monitoring a repository.")
            )
        }
    }

    private func remove() {
        guard let name = selected else { return }
        Task {
            do {
                try await store.removeRepo(name)
            } catch {
                store.notify(error.localizedDescription, severity: .error)
            }
        }
    }
}

private struct RepoListRow: View {
    let entry: BackendState.RepoEntry

    var body: some View {
        HStack {
            Label(entry.shortName, systemImage: "shippingbox")
            Spacer()
            if let error = entry.error {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange)
                    .help(error)
            } else if entry.repo == nil {
                ProgressView().controlSize(.mini)
            }
        }
        .help(entry.name)
    }
}

// MARK: - add repository

private struct AddRepoSheet: View {
    let add: (String) async throws -> String
    let added: (String) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var error: String?
    @State private var checking = false

    private var valid: Bool {
        let parts = name.trimmingCharacters(in: .whitespaces)
            .split(separator: "/", omittingEmptySubsequences: false)
        return parts.count == 2 && !parts[0].isEmpty && !parts[1].isEmpty
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Form {
                TextField("Repository:", text: $name, prompt: Text("owner/name"))
                    .onSubmit(submit)
                    .disabled(checking)
                if let error {
                    Text(error).foregroundStyle(.red)
                }
            }
            .formStyle(.columns)
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button(checking ? "Checking…" : "Add", action: submit)
                    .keyboardShortcut(.defaultAction)
                    .disabled(!valid || checking)
            }
        }
        .padding(20)
        .frame(width: 440)
    }

    private func submit() {
        guard valid, !checking else { return }
        checking = true
        error = nil
        Task {
            do {
                added(try await add(name.trimmingCharacters(in: .whitespaces)))
                dismiss()
            } catch {
                self.error = error.localizedDescription
                checking = false
            }
        }
    }
}

// MARK: - authors

/// The backend's login lists, for notify_for.
private struct Logins: Equatable {
    var mine: [String]
    var bots: [String]
    var watched: [String]
}

private enum LoginGroup: Hashable { case mine, bots, watched }

/// One tab per login list, each a list with +/− buttons. Changes save at once.
struct AuthorSettings: View {
    let store: PrMonStore
    @State private var group = LoginGroup.mine

    private var logins: Logins? {
        guard let config = store.state?.snapshot.config else { return nil }
        return Logins(mine: config.myLogins, bots: config.botLogins, watched: config.watchedLogins)
    }

    var body: some View {
        if !store.isConnected {
            ContentUnavailableView(
                "Not Connected", systemImage: "bolt.horizontal.circle",
                description: Text("Settings are stored by the pr-mon backend.")
            )
        } else if let logins {
            editor(logins)
        } else {
            ProgressView()
        }
    }

    private func editor(_ logins: Logins) -> some View {
        VStack(spacing: 16) {
            Picker("Group", selection: $group) {
                Text("My Logins").tag(LoginGroup.mine)
                Text("Bots").tag(LoginGroup.bots)
                Text("Watched People").tag(LoginGroup.watched)
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()

            list(for: group, logins: logins)
                .id(group)
        }
        .padding(20)
    }

    @ViewBuilder
    private func list(for group: LoginGroup, logins: Logins) -> some View {
        switch group {
        case .mine:
            StringList(
                items: logins.mine, kind: .login, icon: "person.crop.circle",
                emptyTitle: "Using Your gh Login",
                emptyDetail: "Add logins here if you open PRs from more than one account.",
                footer: "PRs by these accounts count as yours in each repository's Notify about."
            ) { save(Logins(mine: $0, bots: logins.bots, watched: logins.watched)) }
        case .bots:
            StringList(
                items: logins.bots, kind: .login, icon: "gearshape.2",
                emptyTitle: "No Extra Bots",
                emptyDetail: "GitHub apps already count as bots. Add accounts that act like one.",
                footer: "PRs by these accounts and by GitHub apps count as bots in Notify about."
            ) { save(Logins(mine: logins.mine, bots: $0, watched: logins.watched)) }
        case .watched:
            StringList(
                items: logins.watched, kind: .login, icon: "eye",
                emptyTitle: "Nobody Watched",
                emptyDetail: "Add people whose PRs you want to hear about.",
                footer: "PRs by these people count as watched in Notify about."
            ) { save(Logins(mine: logins.mine, bots: logins.bots, watched: $0)) }
        }
    }

    private func save(_ logins: Logins) {
        store.saveLogins(mine: logins.mine, bots: logins.bots, watched: logins.watched)
    }
}

/// What a StringList holds.
private enum ListItemKind {
    /// GitHub logins: compared ignoring case, a leading "@" dropped.
    case login
    /// Branch name globs (* any run, ? one character): compared exactly.
    case branchPattern

    var prompt: String {
        switch self {
        case .login: "GitHub login"
        case .branchPattern: "e.g. release-please-*"
        }
    }

    var noun: String {
        switch self {
        case .login: "login"
        case .branchPattern: "pattern"
        }
    }

    var plural: String {
        switch self {
        case .login: "logins"
        case .branchPattern: "patterns"
        }
    }
}

/// A bordered list of strings with +/− buttons beneath it.
private struct StringList: View {
    let items: [String]
    let kind: ListItemKind
    let icon: String
    let emptyTitle: String
    let emptyDetail: String
    var footer: String?
    let update: ([String]) -> Void
    @State private var selected = Set<String>()
    @State private var adding = false

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            VStack(spacing: 0) {
                List(items, id: \.self, selection: $selected) { item in
                    Label(item, systemImage: icon)
                }
                .listStyle(.bordered(alternatesRowBackgrounds: true))
                .onDeleteCommand(perform: remove)
                .overlay {
                    if items.isEmpty {
                        VStack(spacing: 4) {
                            Image(systemName: icon).font(.title2)
                            Text(emptyTitle).font(.headline)
                            Text(emptyDetail).font(.caption).multilineTextAlignment(.center)
                        }
                        .foregroundStyle(.secondary)
                        .padding()
                    }
                }
                HStack(spacing: 0) {
                    Button { adding = true } label: {
                        Image(systemName: "plus").frame(width: 24, height: 20)
                    }
                    .help("Add a \(kind.noun)")
                    .popover(isPresented: $adding, arrowEdge: .bottom) {
                        AddItemPopover(kind: kind, existing: items) { update(items + [$0]) }
                    }
                    Divider().frame(height: 16)
                    Button(action: remove) {
                        Image(systemName: "minus").frame(width: 24, height: 20)
                    }
                    .help("Remove the selected \(kind.plural)")
                    .disabled(selected.isEmpty)
                    Spacer()
                }
                .buttonStyle(.borderless)
                .padding(4)
                .background(.background)
                .overlay(alignment: .top) { Divider() }
            }
            .clipShape(RoundedRectangle(cornerRadius: 6))
            .overlay(RoundedRectangle(cornerRadius: 6).strokeBorder(.separator))

            if let footer {
                Text(footer)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .onChange(of: items) { selected.formIntersection(items) }
    }

    private func remove() {
        guard !selected.isEmpty else { return }
        update(items.filter { !selected.contains($0) })
        selected = []
    }
}

/// Asks for one list item; Return adds it.
private struct AddItemPopover: View {
    let kind: ListItemKind
    let existing: [String]
    let add: (String) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var text = ""
    @FocusState private var focused: Bool

    private var item: String {
        var value = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if kind == .login, value.hasPrefix("@") { value.removeFirst() }
        return value
    }

    private var problem: String? {
        if item.contains(where: \.isWhitespace) { return "A \(kind.noun) has no spaces." }
        let duplicate = existing.contains {
            kind == .login ? $0.caseInsensitiveCompare(item) == .orderedSame : $0 == item
        }
        return duplicate ? "Already in the list." : nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            TextField(kind.prompt, text: $text, prompt: Text(kind.prompt))
                .labelsHidden()
                .focused($focused)
                .onSubmit(submit)
            Text(problem ?? " ")
                .font(.caption)
                .foregroundStyle(.secondary)
            HStack {
                Spacer()
                Button("Add", action: submit)
                    .keyboardShortcut(.defaultAction)
                    .disabled(item.isEmpty || problem != nil)
            }
        }
        .padding(12)
        .frame(width: 240)
        .onAppear { focused = true }
    }

    private func submit() {
        guard !item.isEmpty, problem == nil else { return }
        add(item)
        dismiss()
    }
}

// MARK: - notifications

/// The tabs of a repository's notification settings.
private enum RepoSettingsTab: Hashable, CaseIterable {
    case delivery, events, include, exclude, message

    var title: String {
        switch self {
        case .delivery: "Delivery"
        case .events: "Events"
        case .include: "Include"
        case .exclude: "Exclude"
        case .message: "Message"
        }
    }
}

private struct NotificationSettings: View {
    let store: PrMonStore
    let repo: String
    @Binding var tab: RepoSettingsTab
    @State private var form: NotificationForm?
    @State private var loadError: String?
    /// The settings as last saved (or loaded); edits are compared against these.
    @State private var saved: NotifyConfig?
    @State private var settings: NotifyConfig?
    @State private var preview: NotificationPreview?

    private var notifier: String? { store.state?.snapshot.status.notifier }
    private var twoWay: String? { store.state?.snapshot.status.twoWay }

    private var backendSettings: NotifyConfig? {
        store.state?.snapshot.config.notifications[repo] ?? form?.defaults
    }

    var body: some View {
        Group {
            if let form, let binding = Binding($settings) {
                editor(form: form, settings: binding)
            } else if let loadError {
                ContentUnavailableView("Couldn't Load Settings", systemImage: "exclamationmark.triangle",
                                       description: Text(loadError))
            } else {
                ProgressView()
            }
        }
        .task { await load() }
        .onChange(of: backendSettings) {
            // Another client saved: follow it unless there are unsaved edits here.
            if settings == saved { reset() }
        }
    }

    private func load() async {
        do {
            form = try await store.notificationForm()
            reset()
        } catch {
            loadError = error.localizedDescription
        }
    }

    private func reset() {
        saved = backendSettings
        settings = backendSettings
    }

    private func editor(form: NotificationForm, settings: Binding<NotifyConfig>) -> some View {
        VStack(spacing: 0) {
            Picker("Section", selection: $tab) {
                ForEach(RepoSettingsTab.allCases, id: \.self) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()
            .padding(.top, 16)

            Group {
                switch tab {
                case .delivery: delivery(form: form, settings: settings)
                case .events: events(form: form, settings: settings)
                case .include: include(form: form, settings: settings)
                case .exclude: exclude(settings: settings)
                case .message: message(form: form, settings: settings)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .task(id: settings.wrappedValue.message) { await updatePreview(settings.wrappedValue.message) }

            Divider()
            HStack {
                Button("Send Test") { store.sendTest(repo: repo, settings: outgoing(settings.wrappedValue)) }
                    .help("Send a sample notification with these settings, without saving them")
                Spacer()
                Button("Revert") { reset() }
                    .disabled(settings.wrappedValue == saved)
                Button("Save") {
                    let value = outgoing(settings.wrappedValue)
                    store.saveNotifications(repo: repo, settings: value)
                    saved = settings.wrappedValue
                }
                .keyboardShortcut("s", modifiers: .command)
                .disabled(settings.wrappedValue == saved)
            }
            .padding(12)
        }
    }

    private func delivery(form: NotificationForm, settings: Binding<NotifyConfig>) -> some View {
        Form {
            Section {
                Toggle("Run a script", isOn: settings.scriptEnabled)
                TextField("Command", text: settings.script, prompt: Text("e.g. im --deliver tgram"))
                    .disabled(!settings.wrappedValue.scriptEnabled)
                Toggle("Show a desktop notification", isOn: settings.desktopEnabled)
                    .disabled(notifier == nil)
                Toggle("Send to \(twoWay ?? "a chat") with buttons", isOn: settings.twoWayEnabled)
                    .disabled(twoWay == nil)
            } header: {
                Text("Notify \(repo) by")
            } footer: {
                VStack(alignment: .leading, spacing: 4) {
                    Text(form.scriptHelp)
                    if let notifier {
                        Text("Desktop notifications use \(notifier).")
                    } else {
                        Text("No desktop notifier found (install terminal-notifier).")
                    }
                    if let twoWay {
                        Text("Two-way notifications use \(twoWay).")
                    } else {
                        Text("No two-way channel set up ([two_way] in config.toml).")
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }

    private func events(form: NotificationForm, settings: Binding<NotifyConfig>) -> some View {
        Form {
            Section {
                ForEach(form.events) { event in
                    Toggle(event.label, isOn: eventBinding(event.name, settings: settings, form: form))
                }
                Toggle("Include draft PRs", isOn: settings.includeDrafts)
            } header: {
                Text("When a PR becomes")
            }
        }
        .formStyle(.grouped)
    }

    private func include(form: NotificationForm, settings: Binding<NotifyConfig>) -> some View {
        Form {
            Section {
                ForEach(form.notifyFor) { option in
                    Toggle(option.label, isOn: notifyForBinding(option.name, settings: settings, form: form))
                }
            } header: {
                Text("Notify about")
            } footer: {
                Text("Your logins, extra bots and watched people are set in Settings → Authors. "
                    + "None ticked means everyone's.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }

    private func exclude(settings: Binding<NotifyConfig>) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Branches matching").font(.headline)
            StringList(
                items: settings.wrappedValue.skipBranches, kind: .branchPattern,
                icon: "arrow.triangle.branch",
                emptyTitle: "No Skipped Branches",
                emptyDetail: "PRs from any branch can notify.",
                footer: "Globs: * is any run of characters, ? is one. Without either, a pattern matches branches starting with it."
            ) { settings.wrappedValue.skipBranches = $0 }

            Text("Authors").font(.headline).padding(.top, 8)
            StringList(
                items: settings.wrappedValue.skipAuthors, kind: .login,
                icon: "person.crop.circle.badge.xmark",
                emptyTitle: "No Skipped Authors",
                emptyDetail: "Use this to mute one person, or one bot, that Include lets through.",
                footer: "These PRs never notify, whatever Include says."
            ) { settings.wrappedValue.skipAuthors = $0 }
        }
        .padding(20)
    }

    private func message(form: NotificationForm, settings: Binding<NotifyConfig>) -> some View {
        Form {
            Section {
                TextField("Template", text: settings.message, axis: .vertical)
                    .lineLimit(1...3)
                LabeledContent("Preview") {
                    Text(preview?.text ?? "")
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                if let unknown = preview?.unknown, !unknown.isEmpty {
                    Label("Unknown placeholders: \(unknown.joined(separator: ", "))",
                          systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.orange)
                }
            } header: {
                Text("Message")
            } footer: {
                Text("Placeholders: " + form.variables.map { "{{\($0)}}" }.joined(separator: " "))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
        }
        .formStyle(.grouped)
    }

    /// Without a notifier or a two-way channel that switch is disabled; keep
    /// whatever was saved.
    private func outgoing(_ edited: NotifyConfig) -> NotifyConfig {
        var value = edited
        if notifier == nil, let saved {
            value.desktopEnabled = saved.desktopEnabled
        }
        if twoWay == nil, let saved {
            value.twoWayEnabled = saved.twoWayEnabled
        }
        return value
    }

    /// Events keep the backend's order.
    private func eventBinding(
        _ name: String, settings: Binding<NotifyConfig>, form: NotificationForm
    ) -> Binding<Bool> {
        Binding(
            get: { settings.wrappedValue.events.contains(name) },
            set: { on in
                var chosen = Set(settings.wrappedValue.events)
                if on { chosen.insert(name) } else { chosen.remove(name) }
                settings.wrappedValue.events = form.events.map(\.name).filter(chosen.contains)
            }
        )
    }

    /// Notify-for values keep the backend's order.
    private func notifyForBinding(
        _ name: String, settings: Binding<NotifyConfig>, form: NotificationForm
    ) -> Binding<Bool> {
        Binding(
            get: { settings.wrappedValue.notifyFor.contains(name) },
            set: { on in
                var chosen = Set(settings.wrappedValue.notifyFor)
                if on { chosen.insert(name) } else { chosen.remove(name) }
                settings.wrappedValue.notifyFor = form.notifyFor.map(\.name).filter(chosen.contains)
            }
        )
    }

    private func updatePreview(_ message: String) async {
        try? await Task.sleep(for: .milliseconds(150))  // debounce typing
        guard !Task.isCancelled else { return }
        preview = try? await store.previewNotification(repo: repo, message: message)
    }
}
