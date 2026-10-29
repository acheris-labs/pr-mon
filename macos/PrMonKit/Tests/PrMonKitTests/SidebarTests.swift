// Hiding repositories with no open PRs from the sidebar.

import Testing

@testable import PrMonKit

@Suite struct SidebarTests {
    /// The fixture with Textualize/rich emptied: acme/api has PRs, acme/web
    /// hasn't loaded.
    private func state() throws -> BackendState {
        var snapshot = try Fixtures.snapshot()
        snapshot.repos["Textualize/rich"]?.prs = []
        snapshot.repos["Textualize/rich"]?.prTotal = 0
        return BackendState(snapshot: snapshot)
    }

    private func names(_ groups: [BackendState.OwnerGroup]) -> [String] {
        groups.flatMap { $0.repos.map(\.name) }
    }

    @Test func showsEverythingByDefault() throws {
        #expect(names(try state().owners(hidingEmpty: false)) == ["acme/api", "acme/web", "Textualize/rich"])
    }

    @Test func hidesLoadedReposWithNoPRsAndTheirEmptyGroups() throws {
        let groups = try state().owners(hidingEmpty: true)
        #expect(names(groups) == ["acme/api", "acme/web"])  // acme/web is still loading
        #expect(groups.map(\.owner) == ["acme"])
    }

    @Test func keepsTheSelectedRepo() throws {
        #expect(names(try state().owners(hidingEmpty: true, keeping: "Textualize/rich"))
            == ["acme/api", "acme/web", "Textualize/rich"])
    }

    @Test func keepsAnEmptyRepoWithAnError() throws {
        var snapshot = try state().snapshot
        snapshot.errors["Textualize/rich"] = "rate limited"
        #expect(names(BackendState(snapshot: snapshot).owners(hidingEmpty: true))
            == ["acme/api", "acme/web", "Textualize/rich"])
    }
}
