package dependencies_test

import (
	"reflect"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/dependencies"
	"github.com/acheris-labs/pr-mon/internal/models"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		text   string
		repo   string
		number int
	}{
		{"acme/api#12", "acme/api", 12},
		{" acme/my.repo-x#3 ", "acme/my.repo-x", 3},
		{"https://github.com/acme/api/pull/12", "acme/api", 12},
		{"https://github.com/acme/api/pull/12/files?w=1", "acme/api", 12},
		{"github.com/acme/api/pull/7", "acme/api", 7},
	}
	for _, test := range cases {
		repo, number, err := dependencies.ParseRef(test.text)
		if err != nil || repo != test.repo || number != test.number {
			t.Errorf("ParseRef(%q) = %q, %d, %v", test.text, repo, number, err)
		}
	}
	for _, bad := range []string{"", "acme/api", "#12", "acme/api#0", "acme/api#x",
		"https://github.com/acme/api/issues/12", "https://example.com/acme/api/pull/12"} {
		if _, _, err := dependencies.ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) should fail", bad)
		}
	}
}

func TestAddRefusesSelfDuplicatesAndLoops(t *testing.T) {
	graph := dependencies.Graph{}
	if err := graph.Add("acme/api#1", "acme/web#2"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Add("acme/web#2", "acme/lib#3"); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{
		{"acme/api#1", "acme/api#1"},
		{"acme/api#1", "acme/web#2"},
		{"acme/lib#3", "acme/api#1"}, // 1 → 2 → 3 → 1
		{"acme/web#2", "acme/api#1"},
	} {
		if err := graph.Add(edge[0], edge[1]); err == nil {
			t.Errorf("adding %s → %s should fail", edge[0], edge[1])
		}
	}
	if got := graph.Targets(); !reflect.DeepEqual(got, []string{"acme/lib#3", "acme/web#2"}) {
		t.Errorf("targets = %v", got)
	}
}

func TestRemoveAndRequiredBy(t *testing.T) {
	graph := dependencies.Graph{}
	graph.Add("acme/api#1", "acme/lib#9")
	graph.Add("acme/web#2", "acme/lib#9")
	if got := graph.RequiredBy("acme/lib#9"); !reflect.DeepEqual(got,
		[]string{"acme/api#1", "acme/web#2"}) {
		t.Errorf("required by = %v", got)
	}
	if !graph.Remove("acme/api#1", "acme/lib#9") || graph.Remove("acme/api#1", "acme/lib#9") {
		t.Error("remove should succeed once")
	}
	if _, found := graph["acme/api#1"]; found {
		t.Error("an empty entry should be dropped")
	}
	graph.Forget("acme/web#2")
	if len(graph) != 0 {
		t.Errorf("graph = %v", graph)
	}
}

func TestTreeMarksRepeats(t *testing.T) {
	// A diamond: 1 waits on 2 and 3, and both wait on 4.
	graph := dependencies.Graph{}
	graph.Add("a/r#1", "a/r#2")
	graph.Add("a/r#1", "a/r#3")
	graph.Add("a/r#2", "a/r#4")
	graph.Add("a/r#3", "a/r#4")
	ref := func(key string) models.PRRef {
		repo, number, _ := dependencies.ParseKey(key)
		return models.PRRef{Repo: repo, Number: number}
	}
	up := graph.Tree("a/r#1", true, ref)
	if len(up) != 2 || up[0].PR.Number != 2 || up[1].PR.Number != 3 {
		t.Fatalf("upstream = %+v", up)
	}
	if first := up[0].Children; len(first) != 1 || first[0].PR.Number != 4 || first[0].Repeated {
		t.Errorf("first path to #4 = %+v", first)
	}
	if second := up[1].Children; len(second) != 1 || !second[0].Repeated ||
		len(second[0].Children) != 0 {
		t.Errorf("second path to #4 = %+v", second)
	}
	down := graph.Tree("a/r#4", false, ref)
	if len(down) != 2 || down[0].Children[0].PR.Number != 1 || !down[1].Children[0].Repeated {
		t.Errorf("downstream = %+v", down)
	}
}
