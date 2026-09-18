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
        // Like Mail: the Dock shows how many PRs you haven't looked at, with or
        // without a window open.
        store.unseenTotalChanged = { count in
            NSApp.dockTile.badgeLabel = count > 0 ? String(count) : nil
        }
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
    /// Like any document-less Mac app (Mail, Messages): closing the window leaves
    /// the app running, badge and all, and clicking the Dock icon brings it
    /// back. Quit with Command-Q; the backend keeps running either way.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }
}
