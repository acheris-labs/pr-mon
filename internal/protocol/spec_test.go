// Keeps docs/protocol.md in step with the code: every table in the document is
// compared with the Go types, ops and kinds it describes.

package protocol_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/protocol"
	"github.com/acheris-labs/pr-mon/internal/service"
)

var rowName = regexp.MustCompile("^\\| `([^`]+)` \\|")

func spec(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "..", "docs", "protocol.md"))
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	return string(text)
}

// tableNames are the backticked first-column names of the `index`-th table under
// a heading.
func tableNames(t *testing.T, text, heading string, index int) []string {
	t.Helper()
	lines := strings.Split(text, "\n")
	start := -1
	for number, line := range lines {
		if line == heading {
			start = number + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("the spec has no heading %q", heading)
	}
	tables := [][]string{}
	inTable := false
	for _, line := range lines[start:] {
		switch {
		case strings.HasPrefix(line, "#"):
			if index < len(tables) {
				return tables[index]
			}
			t.Fatalf("the spec has no table %d under %q", index, heading)
		case strings.HasPrefix(line, "|"):
			if !inTable {
				tables = append(tables, []string{})
				inTable = true
			}
			if match := rowName.FindStringSubmatch(line); match != nil {
				tables[len(tables)-1] = append(tables[len(tables)-1], match[1])
			}
		default:
			inTable = false
		}
	}
	if index < len(tables) {
		return tables[index]
	}
	t.Fatalf("the spec has no table %d under %q", index, heading)
	return nil
}

// jsonFields are a type's field names in the order they are encoded.
func jsonFields(t *testing.T, value any) []string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	if _, err := decoder.Token(); err != nil { // the opening brace
		t.Fatal(err)
	}
	names := []string{}
	depth := 0
	for decoder.More() || depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		case string:
			if depth == 0 {
				names = append(names, typed)
				// Skip this field's value.
				if value, err := decoder.Token(); err == nil {
					if delim, isDelim := value.(json.Delim); isDelim &&
						(delim == '{' || delim == '[') {
						depth++
					}
				}
			}
		}
	}
	return names
}

func TestSpecVersion(t *testing.T) {
	text := spec(t)
	want := "Protocol version: **" + itoa(protocol.Version) + "**"
	if !strings.Contains(text, want) {
		t.Errorf("the spec does not say %q", want)
	}
}

func itoa(number int) string {
	return string(rune('0' + number))
}

func TestSpecRequests(t *testing.T) {
	documented := tableNames(t, spec(t), "## Requests", 0)
	if !reflect.DeepEqual(documented, serverOps) {
		t.Errorf("documented ops = %v, want %v", documented, serverOps)
	}
}

func TestSpecEvents(t *testing.T) {
	documented := tableNames(t, spec(t), "## Events", 0)
	if !reflect.DeepEqual(documented, protocol.EventKinds) {
		t.Errorf("documented events = %v, want %v", documented, protocol.EventKinds)
	}
}

func TestSpecActionKinds(t *testing.T) {
	documented := tableNames(t, spec(t), "### Object: `Action`", 1)
	if !reflect.DeepEqual(documented, protocol.ActionKinds) {
		t.Errorf("documented action kinds = %v, want %v", documented, protocol.ActionKinds)
	}
}

func TestSpecObjects(t *testing.T) {
	text := spec(t)
	pid := 1
	cases := []struct {
		name  string
		value any
	}{
		{"Hello", protocol.Hello{}},
		{"Snapshot", protocol.Snapshot{}},
		{"Status", service.Status{PID: &pid}},
		{"Config", config.Config{}},
		{"NotifyConfig", config.NotifyConfig{}},
		{"NotificationForm", notify.Form{}},
		{"EventOption", notify.EventOption{}},
		{"NotificationPreview", notify.Preview{}},
		{"Repo", models.Repo{}},
		{"PullRequest", models.PullRequest{}},
		{"Check", models.Check{}},
		{"AutoMerge", models.AutoMerge{}},
		{"Reason", models.Reason{}},
		{"ArmedMerge", models.ArmedMerge{}},
		{"Action", models.Action{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			documented := tableNames(t, text, "### Object: `"+test.name+"`", 0)
			if got := jsonFields(t, test.value); !reflect.DeepEqual(documented, got) {
				t.Errorf("documented fields = %v, want %v", documented, got)
			}
		})
	}
}

func TestSpecEnumerations(t *testing.T) {
	text := spec(t)
	groups := [][]string{
		config.EventNames,
		{"DRAFT", "CHECKING", "CONFLICT", "FAILING", "PENDING", "BEHIND", "BLOCKED", "READY"},
		{"SQUASH", "MERGE", "REBASE"},
	}
	for _, values := range groups {
		quoted := []string{}
		for _, value := range values {
			quoted = append(quoted, "`"+value+"`")
		}
		if want := strings.Join(quoted, ", "); !strings.Contains(text, want) {
			t.Errorf("the spec does not list %s", want)
		}
	}
}
