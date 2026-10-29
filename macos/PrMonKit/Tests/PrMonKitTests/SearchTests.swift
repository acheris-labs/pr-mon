// Searching open PRs across every repo by title, author or number.

import Testing

@testable import PrMonKit

@Suite struct SearchTests {
    private func found(_ results: [BackendState.SearchResult]) -> [String: [Int]] {
        Dictionary(uniqueKeysWithValues: results.map { ($0.entry.name, $0.prs.map(\.number)) })
    }

    private func state() throws -> BackendState {
        var snapshot = try Fixtures.snapshot()
        snapshot.repos["acme/api"]?.prs[1].author = "Bob-Smith"
        return BackendState(snapshot: snapshot)
    }

    @Test func titleIgnoringCase() throws {
        #expect(found(try state().search("ready TO")) == ["acme/api": [1]])
    }

    @Test func author() throws {
        #expect(found(try state().search("bob")) == ["acme/api": [2]])
    }

    @Test func numberWithHashIsExact() throws {
        #expect(found(try state().search("#4")) == ["acme/api": [4]])
    }

    @Test func bareNumberAlsoMatchesTitles() throws {
        #expect(found(try state().search("41")) == ["Textualize/rich": [41]])
        #expect(found(try state().search("2")) == ["acme/api": [2, 9], "Textualize/rich": [42]])
    }

    @Test func reposKeepConfigOrder() throws {
        #expect(try state().search("pr 4").map(\.entry.name) == ["acme/api", "Textualize/rich"])
    }

    @Test func blankOrUnmatchedFindsNothing() throws {
        #expect(try state().search("  ").isEmpty)
        #expect(try state().search("no such thing").isEmpty)
    }
}
