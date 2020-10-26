package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/muesli/termenv"
)

func main() {
	m := newModel()
	p := tea.NewProgram(m)

	p.EnterAltScreen()
	defer p.ExitAltScreen()

	err := p.Start()
	if err != nil {
		log.Fatalln(err)
	}

	if m.err != nil {
		log.Fatalln(m.err)
	}
}

type model struct {
	spinner  spinner.Model
	viewport viewport.Model
	color    termenv.Profile

	builder  strings.Builder
	ready    bool
	modules  []module
	cursor   int
	updating bool

	err error
}

func newModel() *model {
	s := spinner.NewModel()
	s.ForegroundColor = "2"

	return &model{
		color:   termenv.ColorProfile(),
		spinner: s,
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		spinner.Tick(m.spinner),
		loadCmd(),
	)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		m.err = msg.err
		return m, tea.Quit
	case modulesMsg:
		m.modules = msg.modules
	case updatedMsg:
		m.updating = false
		m.modules = msg.modules
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "enter":
			if !m.updating {
				m.updating = true
				return m, updateCmd(m.modules[m.cursor])
			}
		case "down", "j":
			if !m.updating {
				m.cursor++
				m.fixCursor()
				m.fixViewport(false)
			}
		case "up", "k":
			if !m.updating {
				m.cursor--
				m.fixCursor()
				m.fixViewport(false)
			}
		case "pgup", "u":
			if !m.updating {
				m.viewport.LineUp(1)
				m.fixViewport(true)
			}
		case "pgdown", "d":
			if !m.updating {
				m.viewport.LineDown(1)
				m.fixViewport(true)
			}
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = spinner.Update(msg, m.spinner)
		return m, cmd
	case tea.WindowSizeMsg:
		if !m.ready {
			m.viewport = viewport.Model{
				Width:  msg.Width,
				Height: msg.Height - 2,
			}
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - 2
			m.fixViewport(true)
		}
	}

	return m, nil
}

func (m *model) View() string {
	var header, body, footer string
	if !m.ready || m.modules == nil {
		header = spinner.View(m.spinner) + " Loading..."
	} else if len(m.modules) == 0 {
		header = "All modules are up-to-date"
	} else {
		header = fmt.Sprintf("Press enter to update [%d/%d]", m.cursor+1, len(m.modules))
		m.viewport.SetContent(m.content())
		body = viewport.View(m.viewport)
	}
	footer = "(press 'q' to quit)"

	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

func (m *model) content() string {
	defer m.builder.Reset()

	for i, module := range m.modules {
		cursor := " "
		if m.cursor == i {
			cursor = termenv.String(">").Foreground(m.color.Color("1")).String()
			if m.updating {
				cursor = spinner.View(m.spinner)
			}
		}

		indirect := ""
		if module.Indirect {
			indirect = "// indirect"
		}

		m.builder.WriteString(fmt.Sprintf(
			"%s %s [%s -> %s] %s\n",
			cursor, module.Path, module.Version, module.Update.Version, indirect,
		))
	}

	return m.builder.String()
}

func (m *model) fixCursor() {
	if m.cursor > len(m.modules)-1 {
		m.cursor = 0
	} else if m.cursor < 0 {
		m.cursor = len(m.modules) - 1
	}
}

func (m *model) fixViewport(moveCursor bool) {
	top := m.viewport.YOffset
	bottom := m.viewport.Height + m.viewport.YOffset - 1

	if moveCursor {
		if m.cursor < top {
			m.cursor = top
		} else if m.cursor > bottom {
			m.cursor = bottom
		}
		return
	}

	if m.cursor < top {
		m.viewport.LineUp(top - m.cursor)
	} else if m.cursor > bottom {
		m.viewport.LineDown(m.cursor - bottom)
	}
}

type (
	errMsg struct {
		err error
	}
	modulesMsg struct {
		modules []module
	}
	updatedMsg struct {
		modules []module
	}
)

func loadCmd() tea.Cmd {
	return func() tea.Msg {
		modules, err := load()
		if err != nil {
			return errMsg{err}
		}

		return modulesMsg{modules}
	}
}

func updateCmd(m module) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("go", "get", "-u", m.Update.Path+"@"+m.Update.Version)
		err := cmd.Run()
		if err != nil {
			return errMsg{err}
		}

		modules, err := load()
		if err != nil {
			return errMsg{err}
		}

		return updatedMsg{modules}
	}
}

type module struct {
	Path     string  `json:"Path"`     // module path
	Version  string  `json:"Version"`  // module version
	Update   *module `json:"Update"`   // available update (with -u)
	Main     bool    `json:"Main"`     // is this the main module?
	Indirect bool    `json:"Indirect"` // module is only indirectly needed by main module
}

func load() ([]module, error) {
	cmd := exec.Command("go", "list", "-m", "-u", "-json", "all")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var (
		modules = make([]module, 0)
		dec     = json.NewDecoder(bytes.NewReader(out))
	)
	for {
		var m module
		err := dec.Decode(&m)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		if !m.Main && m.Update != nil {
			modules = append(modules, m)
		}
	}

	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Indirect && !modules[j].Indirect {
			return false
		} else if !modules[i].Indirect && modules[j].Indirect {
			return true
		}
		return modules[i].Path < modules[j].Path
	})

	return modules, nil
}
