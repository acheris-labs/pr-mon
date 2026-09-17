import AppKit
import PrMonKit
import SwiftUI

@main
struct PrMonApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @State private var store: PrMonStore
    @State private var selection = AppSelection()

    init() {
        let store = PrMonStore()
        store.start()
        _store = State(initialValue: store)
    }

    var body: some Scene {
        Window("pr-mon", id: "main") {
            ContentView(store: store, selection: selection)
                .frame(minWidth: 900, minHeight: 480)
        }
        .defaultSize(width: 1200, height: 760)
        .commands {
            CommandGroup(replacing: .newItem) {}
            CommandGroup(before: .toolbar) {
                Button("Refresh All") { store.refreshAll() }
                    .keyboardShortcut("r")
                    .disabled(!store.isConnected)
                Divider()
            }
            PullRequestCommands()
        }

        Settings {
            SettingsView(store: store, selection: selection)
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    /// Like quitting the TUI: closing the window quits; the backend keeps running.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }
}
