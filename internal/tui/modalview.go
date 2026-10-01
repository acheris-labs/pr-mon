// How each dialog draws itself.

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
)

// modalWidth is a dialog's width, padding included; modalInner is its text's.
// A narrow screen gets a dialog as wide as it is, border and all.
func (m *Model) modalWidth() int { return min(max(50, m.width-10), 90, m.width-2) }

func (m *Model) modalInner() int {
	_, sides := m.modalPadding()
	return m.modalWidth() - 2*sides
}

// modalPadding is the space inside a dialog's border, above and beside its
// text. A narrow screen keeps one column and gives the rest to the text.
func (m *Model) modalPadding() (int, int) {
	if m.narrow() {
		return 0, 1
	}
	return 1, 2
}

func (m *Model) modalView() string {
	body := m.modal.view(m)
	width := m.modalWidth()
	return lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(blue).
		Padding(m.modalPadding()).
		Width(width).
		Render(body)
}

// fitInput draws a text field no wider than its dialog.
func fitInput(input *textinput.Model, model *Model) string {
	input.Width = min(60, model.modalInner()-lipgloss.Width(input.Prompt)-1)
	return input.View()
}

// key is a choice in a dialog, and a place to tap for it.
func (m *Model) key(shortcut, label string) string {
	return m.mark(zoneKey+shortcut, bold.Render("["+shortcut+"] ")+label)
}

func unavailable(shortcut, label, reason string) string {
	if reason == "" {
		reason = "not possible now"
	}
	return dim.Render(fmt.Sprintf("[%s] %s — unavailable (%s)", shortcut, label, reason))
}

func (a *actionMenu) view(model *Model) string {
	if a.confirming != nil {
		danger := lipgloss.NewStyle().Bold(true).Foreground(red)
		lines := []string{danger.Render(fmt.Sprintf("Force merge #%d?", a.pr.Number)), ""}
		if option, found := a.option("merge"); found && option.Note != nil {
			lines = append(lines, lipgloss.NewStyle().Foreground(red).Width(model.modalInner()).
				Render("This "+*option.Note+"."))
		}
		lines = append(lines, "It merges past branch protection, as GitHub lets you.")
		return strings.Join(append(lines, "", model.hints("y: force merge   n/esc: back", model.modalInner())), "\n")
	}
	if a.choosing != nil {
		lines := []string{bold.Render("Merge method")}
		for _, choice := range methodKeys {
			if contains(a.methods(), choice.method) {
				lines = append(lines, model.key(choice.key, choice.label))
			}
		}
		return strings.Join(append(lines, "", model.hints("esc: back", model.modalInner())), "\n")
	}
	lines := []string{bold.Render(a.title()), ""}
	for _, slot := range menuSlots {
		option, found := a.option(slot.key)
		if !found {
			continue
		}
		switch {
		case option.Kind == "force_merge":
			danger := lipgloss.NewStyle().Bold(true).Foreground(red)
			line := danger.Render("[" + slot.shortcut + "] " + option.Label)
			if option.Note != nil {
				line += lipgloss.NewStyle().Foreground(red).Render(" (" + *option.Note + ")")
			}
			lines = append(lines, model.mark(zoneKey+slot.shortcut, line))
		case !option.Available:
			lines = append(lines, unavailable(slot.shortcut, option.Label, derefString(option.Reason)))
		case option.Note != nil:
			lines = append(lines, model.key(slot.shortcut, option.Label)+dim.Render(" ("+*option.Note+")"))
		default:
			lines = append(lines, model.key(slot.shortcut, option.Label))
		}
	}
	dependencies := model.key("w", "Dependencies…")
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
		lines = append(lines, "", model.mark(zoneKey+"d",
			fmt.Sprintf("%s Delete remote branch %s", box, a.pr.HeadRef)))
		hint = "d: toggle delete   esc: close"
	}
	return strings.Join(append(lines, "", model.hints(hint, model.modalInner())), "\n")
}

func (a *addRepoModal) view(model *Model) string {
	lines := []string{a.title(), fitInput(&a.input, model)}
	if a.message != "" {
		style := lipgloss.NewStyle().Foreground(red)
		if a.adding {
			style = dim
		}
		lines = append(lines, style.Render(a.message))
	}
	return strings.Join(append(lines, "", model.hints("enter: add   esc: cancel", model.modalInner())), "\n")
}

func (c *confirmModal) view(model *Model) string {
	return c.message + "\n\n" + model.hints("y: yes   n/esc: no", model.modalInner())
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
		lines = append(lines, "Template", fitInput(&n.message, model))
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
			fitInput(&n.script, model), dim.Render(n.form.ScriptHelp))
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
	if n.tab != tabMessage {
		hint = "space: toggle   " + hint
	}
	return strings.Join(append(lines, "", model.hints(hint, model.modalInner())), "\n")
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
