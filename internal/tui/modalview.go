// How each dialog draws itself.

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
)

// modalWidth is a dialog's width, padding included; modalInner is its text's.
func (m *Model) modalWidth() int { return min(max(50, m.width-10), 90) }

func (m *Model) modalInner() int { return m.modalWidth() - 4 }

func (m *Model) modalView() string {
	body := m.modal.view(m)
	width := m.modalWidth()
	return lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(blue).
		Padding(1, 2).
		Width(width).
		Render(body)
}

func key(shortcut, label string) string {
	return bold.Render("["+shortcut+"] ") + label
}

func unavailable(shortcut, label, reason string) string {
	if reason == "" {
		reason = "not possible now"
	}
	return dim.Render(fmt.Sprintf("[%s] %s — unavailable (%s)", shortcut, label, reason))
}

func (a *actionMenu) view(model *Model) string {
	if a.choosing != nil {
		lines := []string{bold.Render("Merge method")}
		for _, choice := range methodKeys {
			if contains(a.methods(), choice.method) {
				lines = append(lines, key(choice.key, choice.label))
			}
		}
		return strings.Join(append(lines, "", dim.Render("esc: back")), "\n")
	}
	lines := []string{bold.Render(a.title()), ""}
	for _, slot := range menuSlots {
		option, found := a.option(slot.key)
		if !found {
			continue
		}
		switch {
		case !option.Available:
			lines = append(lines, unavailable(slot.shortcut, option.Label, derefString(option.Reason)))
		case option.Note != nil:
			lines = append(lines, key(slot.shortcut, option.Label)+dim.Render(" ("+*option.Note+")"))
		default:
			lines = append(lines, key(slot.shortcut, option.Label))
		}
	}
	dependencies := key("w", "Dependencies…")
	if waits := len(a.pr.WaitsOn); waits > 0 {
		dependencies += dim.Render(fmt.Sprintf(" (waits on %d)", waits))
	}
	lines = append(lines, dependencies)
	hint := "esc: close"
	if a.offersDelete() {
		box := "[ ]"
		if a.deleteBranch {
			box = "[x]"
		}
		lines = append(lines, "", fmt.Sprintf("%s Delete remote branch %s", box, a.pr.HeadRef))
		hint = "d: toggle delete   esc: close"
	}
	return strings.Join(append(lines, "", dim.Render(hint)), "\n")
}

func (a *addRepoModal) view(model *Model) string {
	lines := []string{a.title(), a.input.View()}
	if a.message != "" {
		style := lipgloss.NewStyle().Foreground(red)
		if a.adding {
			style = dim
		}
		lines = append(lines, style.Render(a.message))
	}
	return strings.Join(append(lines, "", dim.Render("enter: add   esc: cancel")), "\n")
}

func (c *confirmModal) view(model *Model) string {
	return c.message + "\n\n" + dim.Render("y: yes   n/esc: no")
}

func (n *notificationsModal) view(model *Model) string {
	tabs := []string{}
	for index, name := range tabNames {
		if notifyTab(index) == n.tab {
			tabs = append(tabs, keyStyle.Render(" "+name+" "))
		} else {
			tabs = append(tabs, dim.Render(" "+name+" "))
		}
	}
	lines := []string{bold.Render(n.title()), strings.Join(tabs, ""), ""}
	switch n.tab {
	case tabMessage:
		lines = append(lines, "Template", n.message.View())
		variables := []string{}
		for _, name := range n.form.Variables {
			variables = append(variables, "{{"+name+"}}")
		}
		lines = append(lines, dim.Render("Variables: "+strings.Join(variables, " ")))
		if n.previewErr != "" {
			lines = append(lines, lipgloss.NewStyle().Foreground(red).Render(
				"Preview unavailable: "+n.previewErr))
		} else {
			lines = append(lines, "Preview: "+n.preview_.Text)
			if len(n.preview_.Unknown) > 0 {
				lines = append(lines, lipgloss.NewStyle().Foreground(red).Render(
					"Unknown placeholders: "+strings.Join(n.preview_.Unknown, ", ")))
			}
		}
	case tabEvents:
		lines = append(lines, "Notify when a PR becomes:")
		chosen := map[string]bool{}
		for _, event := range n.settings.Events {
			chosen[event] = true
		}
		for index, option := range n.form.Events {
			lines = append(lines, n.checkbox(chosen[option.Name], option.Label, index == n.eventCursor))
		}
		lines = append(lines, n.checkbox(n.settings.IncludeDrafts, "Include draft PRs",
			n.eventCursor == len(n.form.Events)))
	case tabScript:
		lines = append(lines, n.checkbox(n.settings.ScriptEnabled, "Run script", false),
			n.script.View(), dim.Render(n.form.ScriptHelp))
	case tabDesktop:
		enabled := n.settings.DesktopEnabled && n.notifier != nil
		lines = append(lines, n.checkbox(enabled, "Show desktop notification", false))
		if n.notifier != nil {
			lines = append(lines, dim.Render("Using "+*n.notifier))
		} else {
			lines = append(lines, dim.Render(
				"No notifier found (install terminal-notifier or notify-send)"))
		}
	}
	hint := "ctrl+s: save   ctrl+t: send test   ←/→: tabs   esc: cancel"
	return strings.Join(append(lines, "", dim.Render(hint)), "\n")
}

func (n *notificationsModal) checkbox(checked bool, label string, cursor bool) string {
	box := "[ ]"
	if checked {
		box = "[x]"
	}
	line := box + " " + label
	if cursor {
		return selected.Render(line)
	}
	return line
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// armedFor is a small helper the table uses.
func armedFor(armed map[int]models.ArmedMerge, number int) (models.ArmedMerge, bool) {
	merge, found := armed[number]
	return merge, found
}
