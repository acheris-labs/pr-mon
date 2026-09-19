// Settings (⌘,): appearance and startup, repositories and their notifications, backend control.

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
    case general, repositories, backend
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
            List(repos, id: \.self, selection: $selected) { name in
                RepoListRow(entry: store.state?.entry(name))
                    .listRowSeparator(.hidden)
            }
            .listStyle(.inset)
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
            NotificationSettings(store: store, repo: selected)
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
    let entry: BackendState.RepoEntry?

    var body: some View {
        HStack {
            Text(entry?.name ?? "")
            Spacer()
            if entry?.error != nil {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange)
                    .help(entry?.error ?? "")
            } else if entry?.repo == nil {
                ProgressView().controlSize(.mini)
            }
        }
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

// MARK: - notifications

private struct NotificationSettings: View {
    let store: PrMonStore
    let repo: String
    @State private var form: NotificationForm?
    @State private var loadError: String?
    /// The settings as last saved (or loaded); edits are compared against these.
    @State private var saved: NotifyConfig?
    @State private var settings: NotifyConfig?
    @State private var preview: NotificationPreview?

    private var notifier: String? { store.state?.snapshot.status.notifier }

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
            Form {
                Section {
                    Toggle("Run a script", isOn: settings.scriptEnabled)
                    TextField("Command", text: settings.script, prompt: Text("e.g. im --deliver tgram"))
                        .disabled(!settings.wrappedValue.scriptEnabled)
                    Toggle("Show a desktop notification", isOn: settings.desktopEnabled)
                        .disabled(notifier == nil)
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
                    }
                    .font(.caption)
                    .foregroundStyle(.secondary)
                }

                Section {
                    ForEach(form.events) { event in
                        Toggle(event.label, isOn: eventBinding(event.name, settings: settings, form: form))
                    }
                    Toggle("Include draft PRs", isOn: settings.includeDrafts)
                } header: {
                    Text("When a PR becomes")
                }

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

    /// Without a notifier the desktop switch is disabled; keep whatever was saved.
    private func outgoing(_ edited: NotifyConfig) -> NotifyConfig {
        var value = edited
        if notifier == nil, let saved {
            value.desktopEnabled = saved.desktopEnabled
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

    private func updatePreview(_ message: String) async {
        try? await Task.sleep(for: .milliseconds(150))  // debounce typing
        guard !Task.isCancelled else { return }
        preview = try? await store.previewNotification(repo: repo, message: message)
    }
}
