package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Same palette as tiny's chooser.
var (
	styleBrand  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("84")).Background(lipgloss.Color("22")).Padding(0, 1)
	styleSubtle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

const typeSentinel = "type a name..."

// pickModel is a one-list picker. In the namespace step the last row switches
// to a text field. It does no I/O, so tests drive it with key messages.
type pickModel struct {
	title   string
	note    string
	labels  []string // what is shown
	values  []string // what is returned; "" marks the "type a name" row
	typable bool
	cursor  int
	typing  bool
	input   textinput.Model
	chosen  string
	aborted bool
	height  int    // terminal rows; 0 until known
	errMsg  string // inline validation error while typing
}

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validNamespace reports whether name is a valid namespace (DNS-1123 label).
func validNamespace(name string) bool { return len(name) <= 63 && dns1123Label.MatchString(name) }

// window returns the slice [start,end) of n items to show in a view of rows
// rows, keeping cursor visible and roughly centred. rows <= 0 or >= n shows all.
func window(n, cursor, rows int) (start, end int) {
	if rows <= 0 || rows >= n {
		return 0, n
	}
	start = cursor - rows/2
	if start < 0 {
		start = 0
	}
	if start > n-rows {
		start = n - rows
	}
	return start, start + rows
}

// listRows is how many list rows fit: the header, hints and footer take 6.
func (m pickModel) listRows() int {
	if m.height <= 0 {
		return 0
	}
	if r := m.height - 6; r > 3 {
		return r
	}
	return 3
}

func newPickModel(title, note string, labels, values []string, cursor int, typable bool) pickModel {
	ti := textinput.New()
	ti.Placeholder = "namespace"
	ti.CharLimit = 63
	m := pickModel{title: title, note: note, labels: labels, values: values, cursor: cursor, typable: typable, input: ti}
	if typable {
		m.labels = append(append([]string{}, labels...), typeSentinel)
		m.values = append(append([]string{}, values...), "")
	}
	// Nothing to choose from: go straight to typing.
	if typable && len(values) == 0 {
		m.typing = true
		m.input.Focus()
	}
	return m
}

func (m pickModel) Init() tea.Cmd {
	if m.typing {
		return textinput.Blink
	}
	return nil
}

func (m pickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if w, ok := msg.(tea.WindowSizeMsg); ok {
		m.height = w.Height
		return m, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	key := k.String()
	if key == "ctrl+c" || (key == "esc" && !(m.typing && len(m.values) > 1)) || (key == "q" && !m.typing) {
		m.aborted = true
		return m, tea.Quit
	}
	if m.typing {
		if key == "esc" { // back to the list
			m.typing, m.errMsg = false, ""
			m.input.Blur()
			return m, nil
		}
		if key == "enter" {
			v := strings.TrimSpace(m.input.Value())
			if v == "" {
				return m, nil
			}
			if !validNamespace(v) {
				m.errMsg = "not a valid namespace name (lower-case letters, digits and '-', at most 63)"
				return m, nil
			}
			m.chosen = v
			return m, tea.Quit
		}
		m.errMsg = ""
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	switch key {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.values)-1 {
			m.cursor++
		}
	case "enter":
		if m.values[m.cursor] == "" {
			m.typing = true
			m.input.Focus()
			return m, textinput.Blink
		}
		m.chosen = m.values[m.cursor]
		return m, tea.Quit
	}
	return m, nil
}

func (m pickModel) View() string {
	var b strings.Builder
	b.WriteString(styleBrand.Render("boardingparty") + styleSubtle.Render("  "+m.title) + "\n\n")
	if m.note != "" {
		b.WriteString("  " + styleWarn.Render("! ") + m.note + "\n\n")
	}
	if m.typing {
		b.WriteString("  namespace:\n\n  " + m.input.View() + "\n\n")
		if m.errMsg != "" {
			b.WriteString("  " + styleWarn.Render(m.errMsg) + "\n\n")
		}
		hint := "  enter confirm · esc cancel"
		if len(m.values) > 1 {
			hint = "  enter confirm · esc back to the list"
		}
		b.WriteString(styleSubtle.Render(hint) + "\n")
		return b.String()
	}
	start, end := window(len(m.labels), m.cursor, m.listRows())
	if start > 0 {
		b.WriteString(styleSubtle.Render(fmt.Sprintf("  ↑ %d more", start)) + "\n")
	}
	for i := start; i < end; i++ {
		l := m.labels[i]
		mark := "  "
		if i == m.cursor {
			mark = styleKey.Render("▸ ")
		}
		b.WriteString(mark + l + "\n")
	}
	if end < len(m.labels) {
		b.WriteString(styleSubtle.Render(fmt.Sprintf("  ↓ %d more", len(m.labels)-end)) + "\n")
	}
	b.WriteString("\n" + styleSubtle.Render("  ↑/↓ move · enter choose · esc cancel") + "\n")
	return b.String()
}

// termPrompter runs the pickers on the terminal. The UI goes to stderr so
// stdout stays clean for command output.
type termPrompter struct{}

func (termPrompter) run(m pickModel) (string, error) {
	out, err := tea.NewProgram(m, tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return "", err
	}
	final, ok := out.(pickModel)
	if !ok {
		return "", fmt.Errorf("picker returned unexpected model %T", out)
	}
	if final.aborted {
		return "", errCancelled
	}
	return final.chosen, nil
}

func (p termPrompter) pickContext(contexts []string, cursor int) (string, error) {
	return p.run(newPickModel("pick the cluster", "", contexts, contexts, cursor, false))
}

func (p termPrompter) pickNamespace(ctxName string, items []nsItem, cursor int, note string) (string, error) {
	labels := make([]string, len(items))
	values := make([]string, len(items))
	for i, it := range items {
		labels[i], values[i] = it.Name, it.Name
		if it.Ready {
			labels[i] += styleSubtle.Render("  (tiny set up)")
		}
	}
	return p.run(newPickModel("pick the namespace on "+ctxName, note, labels, values, cursor, true))
}
