// A repo's dependency graph, shown in the detail column when no PR is selected:
// its open PRs in rows by merge order, with an arrow from each PR to the ones
// waiting on it. PRs outside the repo's open ones sit dimmed in the top row.

import PrMonKit
import SwiftUI

/// Where each PR goes: rows from the first to merge to the last.
struct RepoGraphLayout {
    struct Edge: Hashable {
        var from: String
        var to: String
    }

    var rows: [[PRRef]] = []
    var edges: [Edge] = []
    var unlinked: [PullRequest] = []

    init(repo: Repo) {
        let open = Dictionary(uniqueKeysWithValues: repo.prs.map { (ref(repo.name, $0).key, $0) })
        var outside: [String: PRRef] = [:]
        for pr in repo.prs {
            for dependency in pr.waitsOn where open[dependency.key] == nil {
                outside[dependency.key] = dependency
            }
        }
        // Outside PRs are row 0; a repo PR goes one row below the deepest PR it
        // waits on.
        var depth: [String: Int] = [:]
        func depthOf(_ key: String) -> Int {
            if let known = depth[key] { return known }
            guard let pr = open[key] else { return 0 }
            depth[key] = 1 // a loop, which the backend refuses, stops here
            let deepest = pr.waitsOn.map { depthOf($0.key) + 1 }.max() ?? 1
            depth[key] = max(1, deepest)
            return depth[key]!
        }
        var rows: [Int: [PRRef]] = [:]
        if !outside.isEmpty {
            rows[0] = outside.values.sorted { $0.key < $1.key }
        }
        for pr in repo.prs {
            if pr.waitsOn.isEmpty && pr.requiredBy.isEmpty {
                unlinked.append(pr)
                continue
            }
            let key = ref(repo.name, pr).key
            rows[depthOf(key), default: []].append(ref(repo.name, pr))
            edges += pr.waitsOn.map { Edge(from: $0.key, to: key) }
        }
        self.rows = rows.keys.sorted().map { rows[$0]! }
        orderRows()
    }

    var isEmpty: Bool { rows.isEmpty }

    /// Sorts each row by where the PRs it waits on sit in the rows above, so
    /// arrows cross less.
    private mutating func orderRows() {
        var column: [String: Double] = [:]
        let edges = edges
        for index in rows.indices {
            if index > 0 {
                let place = { (ref: PRRef) -> Double in
                    let parents = edges.filter { $0.to == ref.key }.compactMap { column[$0.from] }
                    return parents.isEmpty ? .infinity : parents.reduce(0, +) / Double(parents.count)
                }
                rows[index].sort { place($0) < place($1) }
            }
            for (position, ref) in rows[index].enumerated() {
                column[ref.key] = Double(position)
            }
        }
    }

    private func ref(_ repo: String, _ pr: PullRequest) -> PRRef {
        PRRef(repo: repo, number: pr.number, title: pr.title, url: pr.url, state: .open, status: pr.status)
    }
}

struct RepoGraphView: View {
    let repo: Repo
    /// Selects one of the repo's PRs.
    let select: (Int) -> Void

    private static let nodeSize = CGSize(width: 220, height: 64)
    private static let spacing = CGSize(width: 24, height: 48)
    private static let margin: CGFloat = 20

    var body: some View {
        let layout = RepoGraphLayout(repo: repo)
        if layout.isEmpty {
            ContentUnavailableView("No Dependencies", systemImage: "point.3.connected.trianglepath.dotted",
                                   description: Text("None of these pull requests waits on another."))
        } else {
            ScrollView([.horizontal, .vertical]) {
                VStack(alignment: .leading, spacing: 24) {
                    graph(layout)
                    if !layout.unlinked.isEmpty {
                        unlinked(layout.unlinked)
                    }
                }
                .padding(Self.margin)
            }
            .navigationTitle(repo.name)
        }
    }

    private func graph(_ layout: RepoGraphLayout) -> some View {
        let widest = layout.rows.map(\.count).max() ?? 1
        let width = CGFloat(widest) * Self.nodeSize.width + CGFloat(widest - 1) * Self.spacing.width
        let height = CGFloat(layout.rows.count) * Self.nodeSize.height
            + CGFloat(layout.rows.count - 1) * Self.spacing.height
        var centres: [String: CGPoint] = [:]
        for (row, refs) in layout.rows.enumerated() {
            let rowWidth = CGFloat(refs.count) * Self.nodeSize.width + CGFloat(refs.count - 1) * Self.spacing.width
            let start = (width - rowWidth) / 2
            for (index, ref) in refs.enumerated() {
                centres[ref.key] = CGPoint(
                    x: start + CGFloat(index) * (Self.nodeSize.width + Self.spacing.width) + Self.nodeSize.width / 2,
                    y: CGFloat(row) * (Self.nodeSize.height + Self.spacing.height) + Self.nodeSize.height / 2
                )
            }
        }
        return ZStack(alignment: .topLeading) {
            Canvas { context, _ in
                for edge in layout.edges {
                    guard let from = centres[edge.from], let to = centres[edge.to] else { continue }
                    draw(edge: from, to, in: context)
                }
            }
            ForEach(layout.rows.flatMap { $0 }) { ref in
                RepoGraphNode(ref: ref, repo: repo.name, outside: !isOpen(ref)) {
                    select(ref.number)
                }
                .frame(width: Self.nodeSize.width, height: Self.nodeSize.height)
                .position(centres[ref.key] ?? .zero)
            }
        }
        .frame(width: width, height: height)
    }

    private func isOpen(_ ref: PRRef) -> Bool {
        ref.repo == repo.name && repo.prs.contains { $0.number == ref.number }
    }

    /// A curve from the bottom of the PR that merges first to the top of the
    /// one waiting on it, ending in an arrowhead.
    private func draw(edge from: CGPoint, _ to: CGPoint, in context: GraphicsContext) {
        let start = CGPoint(x: from.x, y: from.y + Self.nodeSize.height / 2)
        let end = CGPoint(x: to.x, y: to.y - Self.nodeSize.height / 2 - 2)
        let bend = (end.y - start.y) / 2
        var line = Path()
        line.move(to: start)
        line.addCurve(to: end, control1: CGPoint(x: start.x, y: start.y + bend),
                      control2: CGPoint(x: end.x, y: end.y - bend))
        context.stroke(line, with: .color(.secondary), lineWidth: 1.5)
        var head = Path()
        head.move(to: end)
        head.addLine(to: CGPoint(x: end.x - 5, y: end.y - 8))
        head.addLine(to: CGPoint(x: end.x + 5, y: end.y - 8))
        head.closeSubpath()
        context.fill(head, with: .color(.secondary))
    }

    private func unlinked(_ prs: [PullRequest]) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Not Linked").font(.headline)
            ForEach(prs) { pr in
                Button { select(pr.number) } label: {
                    PRRefRow(ref: PRRef(repo: repo.name, number: pr.number, title: pr.title, url: pr.url,
                                        state: .open, status: pr.status), repo: repo.name)
                }
                .buttonStyle(.plain)
            }
        }
    }
}

/// One PR in the graph: its status, number and title. Clicking a repo PR
/// selects it; PRs outside the repo open on GitHub.
private struct RepoGraphNode: View {
    let ref: PRRef
    let repo: String
    let outside: Bool
    let select: () -> Void

    var body: some View {
        Button {
            if outside { Browser.open(ref.url) } else { select() }
        } label: {
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 6) {
                    Image(systemName: ref.symbol).foregroundStyle(ref.color)
                    Text(ref.label(in: repo)).fontWeight(.semibold)
                    Spacer(minLength: 0)
                    Text(ref.stateTitle)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
                Text(ref.title.isEmpty ? " " : ref.title)
                    .font(.callout)
                    .lineLimit(2)
                    .truncationMode(.tail)
                Spacer(minLength: 0)
            }
            .padding(8)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .background(.background, in: RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(ref.color.opacity(0.6), lineWidth: 1.5))
            .contentShape(RoundedRectangle(cornerRadius: 8))
        }
        .buttonStyle(.plain)
        .opacity(outside ? 0.55 : 1)
        .help(outside ? "Open \(ref.key) on GitHub" : "Select #\(ref.number)")
    }
}
