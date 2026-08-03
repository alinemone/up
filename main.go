package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const appName = "up"

type Config struct {
	Services map[string]Service  `json:"services"`
	Groups   map[string][]string `json:"groups"`
}

type Service struct {
	CWD      string            `json:"cwd,omitempty"`
	Command  string            `json:"command"`
	Port     int               `json:"port,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Restart  bool              `json:"restart"`
	Schedule Schedule          `json:"schedule,omitempty"`
}

type Schedule struct {
	At    string `json:"at,omitempty"`
	Every string `json:"every,omitempty"`
}

type RuntimeState struct {
	Name      string
	Status    string
	PID       int
	Port      int
	Command   string
	CWD       string
	StartedAt time.Time
	NextRun   time.Time
	Restarts  int
	LastExit  string
}

type RuntimeLog struct {
	Time    time.Time
	Service string
	Stream  string
	Message string
}

type Monitor struct {
	mu     sync.Mutex
	states map[string]RuntimeState
	logs   []RuntimeLog
}

const maxLogs = 14

func newMonitor(names []string, cfg Config) *Monitor {
	m := &Monitor{states: make(map[string]RuntimeState)}
	for _, name := range names {
		svc := cfg.Services[name]
		m.states[name] = RuntimeState{
			Name:    name,
			Status:  "queued",
			Port:    svc.Port,
			Command: svc.Command,
			CWD:     svc.CWD,
		}
	}
	return m
}

func (m *Monitor) update(name string, fn func(*RuntimeState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[name]
	if state.Name == "" {
		state.Name = name
	}
	fn(&state)
	m.states[name] = state
}

func (m *Monitor) remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, name)
}

func (m *Monitor) addLog(name, stream, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, RuntimeLog{
		Time:    time.Now(),
		Service: name,
		Stream:  stream,
		Message: message,
	})
	if len(m.logs) > maxLogs {
		m.logs = append([]RuntimeLog(nil), m.logs[len(m.logs)-maxLogs:]...)
	}
}

func (m *Monitor) clearLogs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = nil
}

func (m *Monitor) snapshot() ([]RuntimeState, []RuntimeLog) {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := make([]RuntimeState, 0, len(m.states))
	for _, state := range m.states {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	logs := append([]RuntimeLog(nil), m.logs...)
	return states, logs
}

type Runner struct {
	ctx  context.Context
	cfg  Config
	mon  *Monitor
	mu   sync.Mutex
	runs map[string]*runHandle
}

type runHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newRunner(ctx context.Context, cfg Config, mon *Monitor) *Runner {
	return &Runner{
		ctx:  ctx,
		cfg:  cfg,
		mon:  mon,
		runs: make(map[string]*runHandle),
	}
}

func (r *Runner) Start(name string) error {
	r.mu.Lock()
	svc, ok := r.cfg.Services[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("service %q not found", name)
	}
	if _, running := r.runs[name]; running {
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	r.runs[name] = &runHandle{cancel: cancel, done: done}
	r.mu.Unlock()

	r.mon.update(name, func(state *RuntimeState) {
		state.Status = "queued"
		state.PID = 0
		state.LastExit = ""
		state.Port = svc.Port
		state.Command = svc.Command
		state.CWD = svc.CWD
	})

	go func() {
		defer close(done)
		supervise(ctx, r.mon, name, svc)
		r.mu.Lock()
		delete(r.runs, name)
		r.mu.Unlock()
		if ctx.Err() != nil {
			r.mon.addLog(name, "sys", "stopped by user")
			r.mon.remove(name)
		}
	}()
	return nil
}

func (r *Runner) UpdateConfig(cfg Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
}

func (r *Runner) Stop(name string) {
	r.mu.Lock()
	handle := r.runs[name]
	r.mu.Unlock()
	if handle != nil {
		r.mon.update(name, func(state *RuntimeState) {
			state.Status = "stopping"
			state.LastExit = ""
		})
		handle.cancel()
	}
}

func (r *Runner) Restart(name string) {
	r.mu.Lock()
	handle := r.runs[name]
	r.mu.Unlock()
	if handle == nil {
		_ = r.Start(name)
		return
	}
	r.mon.update(name, func(state *RuntimeState) {
		state.Status = "restarting"
		state.LastExit = ""
	})
	handle.cancel()
	go func() {
		select {
		case <-handle.done:
		case <-time.After(8 * time.Second):
		}
		_ = r.Start(name)
	}()
}

func (r *Runner) StopAll() {
	type namedHandle struct {
		name   string
		handle *runHandle
	}
	r.mu.Lock()
	handles := make([]namedHandle, 0, len(r.runs))
	for name, handle := range r.runs {
		handles = append(handles, namedHandle{name: name, handle: handle})
	}
	r.mu.Unlock()
	for _, item := range handles {
		r.mon.update(item.name, func(state *RuntimeState) {
			state.Status = "stopping"
			state.LastExit = ""
		})
		item.handle.cancel()
	}
	deadline := time.After(8 * time.Second)
	for _, item := range handles {
		select {
		case <-item.handle.done:
		case <-deadline:
			return
		}
	}
}

func (r *Runner) IsRunning(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.runs[name]
	return ok
}

type tickMsg time.Time
type shutdownDoneMsg struct{}

type appModel struct {
	cfg       Config
	cfgPath   string
	mon       *Monitor
	runner    *Runner
	initial   []string
	services  []string
	groups    []string
	tab       string
	cursor    int
	groupCur  int
	width     int
	height    int
	addMode   bool
	addCursor int
	addMarked map[string]bool
	addItems  []addItem
	form      *serviceForm
	groupForm *groupForm
	returnAdd bool
	shutdown  bool
	pulse     int
	message   string
}

type addItem struct {
	Kind string
	Name string
}

type serviceForm struct {
	editName string
	focus    int
	inputs   []textinput.Model
	restart  bool
}

type groupForm struct {
	editName  string
	name      textinput.Model
	services  []string
	marked    map[string]bool
	cursor    int
	nameFocus bool
}

var (
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	keyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Bold(true)
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Bold(true)
	greenStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	yellowStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	redStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	logoStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("210")).Bold(true)
	cyanStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Bold(true)
	panelStyle    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238")).
			Padding(0, 1)
	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238")).
			Padding(0, 1)
	inputFocusStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("45")).
			Padding(0, 1)
)

func newAppModel(cfg Config, mon *Monitor, runner *Runner, initial []string) appModel {
	services := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		services = append(services, name)
	}
	sort.Strings(services)
	groups := make([]string, 0, len(cfg.Groups))
	for name := range cfg.Groups {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	path, _ := configPath()
	tab := "monitor"
	return appModel{
		cfg:      cfg,
		cfgPath:  path,
		mon:      mon,
		runner:   runner,
		initial:  initial,
		services: services,
		groups:   groups,
		tab:      tab,
		width:    100,
		height:   30,
	}
}

func (m appModel) Init() tea.Cmd {
	for _, name := range m.initial {
		if err := m.runner.Start(name); err != nil {
			m.mon.addLog(name, "err", err.Error())
		}
	}
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m appModel) shutdownCmd() tea.Cmd {
	return func() tea.Msg {
		m.runner.StopAll()
		return shutdownDoneMsg{}
	}
}

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		if m.shutdown {
			m.pulse = (m.pulse + 1) % 4
		}
		return m, tickCmd()
	case shutdownDoneMsg:
		return m, tea.Quit
	case tea.KeyMsg:
		key := msg.String()
		if m.shutdown {
			return m, nil
		}
		if m.form != nil {
			return m.updateServiceForm(msg)
		}
		if m.groupForm != nil {
			return m.updateGroupForm(msg)
		}
		if m.addMode {
			return m.updateAddMode(key)
		}
		switch key {
		case "q", "ctrl+c":
			m.shutdown = true
			m.message = "stopping services"
			return m, m.shutdownCmd()
		case "up", "k":
			if m.tab == "groups" {
				if m.groupCur > 0 {
					m.groupCur--
				}
			} else if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.tab == "monitor" {
				if m.cursor < len(m.activeNames())-1 {
					m.cursor++
				}
			} else if m.tab == "services" {
				if m.cursor < len(m.services)-1 {
					m.cursor++
				}
			} else if m.tab == "groups" {
				if m.groupCur < len(m.groups)-1 {
					m.groupCur++
				}
			}
		case "a":
			m.openAddMenu("select service(s) to start")
		case "n":
			m.form = newServiceForm("", Service{Restart: true})
			m.message = ""
		case "c":
			m.mon.clearLogs()
			m.message = "logs cleared"
		case "s":
			name := m.selectedServiceForAction()
			if name != "" {
				m.runner.Stop(name)
				m.message = "stopping " + name
			}
		case "r":
			if m.tab == "groups" {
				group := m.selectedGroup()
				if group != "" {
					for _, name := range m.cfg.Groups[group] {
						_ = m.runner.Start(name)
					}
					m.tab = "monitor"
					m.message = "starting group " + group
				}
			} else if name := m.selectedServiceForAction(); name != "" {
				if m.runner.IsRunning(name) {
					m.runner.Restart(name)
					m.message = "restarting " + name
				} else if err := m.runner.Start(name); err != nil {
					m.message = err.Error()
				} else {
					m.tab = "monitor"
					m.message = "starting " + name
				}
			}
		case "e":
			if m.tab == "groups" {
				group := m.selectedGroup()
				if group != "" {
					m.groupForm = newGroupForm(group, m.services, m.cfg.Groups[group])
					m.message = ""
				}
			} else {
				name := m.selectedActiveService()
				if name == "" {
					name = m.selectedConfigService()
				}
				if name != "" {
					m.form = newServiceForm(name, m.cfg.Services[name])
					m.message = ""
				}
			}
		case "g":
			m.groupForm = newGroupForm("", m.services, m.activeNames())
			m.message = ""
		case "d":
			if m.tab == "services" {
				name := m.selectedConfigService()
				if name != "" {
					delete(m.cfg.Services, name)
					for group, members := range m.cfg.Groups {
						m.cfg.Groups[group] = without(members, name)
					}
					_ = saveConfig(m.cfg)
					m.refreshConfigLists()
					m.message = "deleted " + name
				}
			}
		case "enter":
			if m.tab == "groups" {
				group := m.selectedGroup()
				if group != "" {
					for _, name := range m.cfg.Groups[group] {
						_ = m.runner.Start(name)
					}
					m.tab = "monitor"
					m.message = "starting group " + group
				}
			} else if name := m.selectedServiceForAction(); name != "" {
				if m.tab == "monitor" {
					return m, nil
				} else if m.tab == "services" {
					if err := m.runner.Start(name); err != nil {
						m.message = err.Error()
					} else {
						m.tab = "monitor"
						m.message = "starting " + name
					}
				}
			}
		}
	}
	return m, nil
}

func (m appModel) updateAddMode(key string) (tea.Model, tea.Cmd) {
	choices := m.addItems
	switch key {
	case "esc", "q":
		m.addMode = false
		m.message = ""
	case "n":
		m.addMode = false
		m.returnAdd = true
		m.form = newServiceForm("", Service{Restart: true})
		m.message = ""
	case "g":
		m.addMode = false
		m.returnAdd = true
		m.groupForm = newGroupForm("", m.services, nil)
		m.message = ""
	case "e":
		if len(choices) == 0 {
			return m, nil
		}
		item := choices[m.addCursor]
		m.addMode = false
		m.returnAdd = true
		if item.Kind == "group" {
			m.groupForm = newGroupForm(item.Name, m.services, m.cfg.Groups[item.Name])
		} else {
			m.form = newServiceForm(item.Name, m.cfg.Services[item.Name])
		}
		m.message = ""
	case "up", "k":
		if m.addCursor > 0 {
			m.addCursor--
		}
	case "down", "j":
		if m.addCursor < len(choices)-1 {
			m.addCursor++
		}
	case "enter":
		if len(choices) > 0 {
			toStart := make([]string, 0)
			for key, marked := range m.addMarked {
				if marked {
					toStart = append(toStart, m.servicesForAddKey(key)...)
				}
			}
			if len(toStart) == 0 {
				toStart = append(toStart, m.servicesForAddItem(choices[m.addCursor])...)
			}
			toStart = uniqueNotRunning(toStart, m.runner)
			if len(toStart) == 0 {
				m.message = "nothing to start"
				return m, nil
			}
			for _, name := range toStart {
				if err := m.runner.Start(name); err != nil {
					m.message = err.Error()
					return m, nil
				}
			}
			if len(toStart) == 1 {
				m.message = "starting 1 service"
			} else {
				m.message = fmt.Sprintf("starting %d services", len(toStart))
			}
		}
		m.addMode = false
	case " ":
		if len(choices) > 0 {
			item := choices[m.addCursor]
			services := uniqueNotRunning(m.servicesForAddItem(item), m.runner)
			if len(services) == 0 {
				m.message = item.Name + " is already running"
				return m, nil
			}
			key := addItemKey(item)
			if m.addMarked == nil {
				m.addMarked = make(map[string]bool)
			}
			m.addMarked[key] = !m.addMarked[key]
		}
	}
	return m, nil
}

func newServiceForm(editName string, svc Service) *serviceForm {
	values := []string{editName, svc.CWD, svc.Command, "", fmt.Sprintf("%v", svc.Restart), svc.Schedule.At, svc.Schedule.Every}
	placeholders := []string{"my-api", "F:\\projects\\api or /home/me/api", "npm run dev", "8080", "true", "09:00", "2h"}
	if svc.Port > 0 {
		values[3] = strconv.Itoa(svc.Port)
	}
	inputs := make([]textinput.Model, len(values))
	for i := range values {
		input := textinput.New()
		input.Prompt = ""
		input.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
		input.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
		input.Placeholder = placeholders[i]
		input.SetValue(values[i])
		input.CharLimit = 500
		input.Width = 70
		if i == 0 {
			input.Focus()
		}
		inputs[i] = input
	}
	return &serviceForm{editName: editName, inputs: inputs, restart: svc.Restart}
}

func (m appModel) updateServiceForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		m.form = nil
		if m.returnAdd {
			m.returnAdd = false
			m.openAddMenu("")
		} else {
			m.message = ""
		}
		return m, nil
	case "ctrl+s":
		if err := m.saveServiceForm(); err != nil {
			m.message = err.Error()
		} else {
			m.form = nil
			m.refreshConfigLists()
			if m.returnAdd {
				m.returnAdd = false
				m.openAddMenu("service saved")
			}
		}
		return m, nil
	case "tab":
		m.form.inputs[m.form.focus].Blur()
		m.form.focus = (m.form.focus + 1) % len(m.form.inputs)
		m.form.inputs[m.form.focus].Focus()
		return m, nil
	case "shift+tab":
		m.form.inputs[m.form.focus].Blur()
		m.form.focus--
		if m.form.focus < 0 {
			m.form.focus = len(m.form.inputs) - 1
		}
		m.form.inputs[m.form.focus].Focus()
		return m, nil
	}
	var cmd tea.Cmd
	m.form.inputs[m.form.focus], cmd = m.form.inputs[m.form.focus].Update(msg)
	return m, cmd
}

func (m *appModel) saveServiceForm() error {
	oldName := m.form.editName
	values := make([]string, len(m.form.inputs))
	for i := range m.form.inputs {
		values[i] = strings.TrimSpace(m.form.inputs[i].Value())
	}
	name := values[0]
	if err := validateName(name); err != nil {
		return err
	}
	if name == "" || values[2] == "" {
		return errors.New("name and command are required")
	}
	port := 0
	if values[3] != "" {
		parsed, err := strconv.Atoi(values[3])
		if err != nil || parsed <= 0 {
			return errors.New("port must be a positive number")
		}
		port = parsed
	}
	restart := true
	if values[4] != "" {
		parsed, err := strconv.ParseBool(values[4])
		if err != nil {
			return errors.New("restart must be true or false")
		}
		restart = parsed
	}
	svc := Service{
		CWD:     values[1],
		Command: values[2],
		Port:    port,
		Restart: restart,
		Schedule: Schedule{
			At:    values[5],
			Every: values[6],
		},
	}
	if err := validateSchedule(svc.Schedule); err != nil {
		return err
	}
	wasRunning := false
	if oldName != "" {
		wasRunning = m.runner.IsRunning(oldName)
	} else {
		wasRunning = m.runner.IsRunning(name)
	}
	if oldName != "" && oldName != name {
		delete(m.cfg.Services, oldName)
		for group, members := range m.cfg.Groups {
			for i, member := range members {
				if member == oldName {
					m.cfg.Groups[group][i] = name
				}
			}
		}
	}
	m.cfg.Services[name] = svc
	if err := saveConfig(m.cfg); err != nil {
		return err
	}
	m.runner.UpdateConfig(m.cfg)
	if wasRunning {
		if oldName != "" && oldName != name {
			m.runner.Stop(oldName)
			go func() {
				time.Sleep(700 * time.Millisecond)
				_ = m.runner.Start(name)
			}()
		} else {
			m.runner.Restart(name)
		}
		m.message = "service saved and restarted"
	} else {
		m.message = "service saved"
	}
	return nil
}

func (m appModel) View() string {
	if m.form != nil {
		return m.renderServiceForm()
	}
	if m.groupForm != nil {
		return m.renderGroupForm()
	}
	if m.addMode {
		return m.renderAddPage()
	}
	states, logs := m.mon.snapshot()
	stateByName := make(map[string]RuntimeState, len(states))
	for _, state := range states {
		stateByName[state.Name] = state
	}
	if len(m.services) == 0 {
		return "No services configured\n"
	}
	if m.cursor >= len(states) && len(states) > 0 {
		m.cursor = len(states) - 1
	}

	var b strings.Builder
	helpPanel := m.panel(m.helpText())
	messageHeight := 0
	if m.message != "" {
		messageText := m.message
		if m.shutdown {
			messageText += strings.Repeat(".", m.pulse)
		}
		message := cyanStyle.Render(messageText) + "\n"
		messageHeight = lipgloss.Height(message)
		b.WriteString(message)
	}

	if m.tab == "monitor" {
		return m.renderMonitor(stateByName, logs, b.String(), helpPanel, messageHeight)
	}

	var servicesPanel string
	switch m.tab {
	case "services":
		servicesPanel = m.panel(m.renderServicesView())
	case "groups":
		servicesPanel = m.panel(m.renderGroupsView())
	default:
		servicesPanel = m.panel(m.renderTable(stateByName))
	}
	b.WriteString(servicesPanel + "\n")
	return m.withBottomHelp(b.String(), helpPanel)
}

func (m appModel) renderMonitor(stateByName map[string]RuntimeState, logs []RuntimeLog, message, helpPanel string, messageHeight int) string {
	helpHeight := lipgloss.Height(helpPanel)
	gapBeforeHelp := 1
	if m.height < 14 {
		gapBeforeHelp = 0
	}
	available := m.height - helpHeight - messageHeight - gapBeforeHelp
	if available < 4 {
		table := m.renderTableLimited(stateByName, -1, false)
		return m.withBottomHelp(message+m.viewportPanel(table, max(3, available), 0, false)+"\n", helpPanel)
	}

	minServiceHeight := 6
	minLogHeight := 6
	tableNoLogo := m.renderTableLimited(stateByName, -1, false)
	naturalServiceHeight := lipgloss.Height(tableNoLogo) + 2
	serviceHeight := min(naturalServiceHeight, available)
	logHeight := 0

	if len(logs) > 0 && available >= minServiceHeight+1+minLogHeight {
		serviceHeight = min(naturalServiceHeight, available-minLogHeight-1)
		if serviceHeight < minServiceHeight {
			serviceHeight = minServiceHeight
		}
		logHeight = available - serviceHeight - 1
	}

	showLogo := m.width >= 110 && serviceHeight >= 10 && serviceHeight >= naturalServiceHeight
	tableContent := m.renderTableLimited(stateByName, -1, showLogo)
	serviceYOffset := max(0, m.cursor-max(0, serviceHeight-6))
	servicePanel := m.viewportPanel(tableContent, serviceHeight, serviceYOffset, false)

	body := message + servicePanel + "\n"
	if logHeight >= minLogHeight {
		logPanel := m.viewportPanel(m.renderLogsContent(logs), logHeight, 0, true)
		body = message + servicePanel + "\n\n" + logPanel + "\n"
	}
	return m.withBottomHelp(body, helpPanel)
}

func (m appModel) viewportPanel(content string, totalHeight, yOffset int, bottom bool) string {
	panelWidth := max(48, m.width-2)
	viewportHeight := max(1, totalHeight-2)
	viewportWidth := max(1, panelWidth-4)
	vp := viewport.New(viewportWidth, viewportHeight)
	vp.SetContent(strings.TrimRight(content, "\n"))
	if bottom {
		vp.GotoBottom()
	} else if yOffset > 0 {
		vp.SetYOffset(yOffset)
	}
	return panelStyle.Width(panelWidth).Height(viewportHeight).Render(vp.View())
}

func (m appModel) renderTabs() string {
	tabs := []string{"services", "groups", "monitor"}
	parts := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		label := strings.ToUpper(tab)
		if tab == m.tab {
			parts = append(parts, selectedStyle.Render("["+label+"]"))
		} else {
			parts = append(parts, mutedStyle.Render(" "+label+" "))
		}
	}
	return strings.Join(parts, " ")
}

func (m appModel) renderServicesView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Services") + "\n")
	if len(m.services) == 0 {
		b.WriteString(mutedStyle.Render("No services. Press 'a' to add one.") + "\n")
		return b.String()
	}
	compact := m.width < 96
	if compact {
		b.WriteString(strings.Join([]string{padCell("", 2), padCell("NAME", 18), padCell("PORT", 8), "COMMAND"}, " ") + "\n")
	} else {
		b.WriteString(strings.Join([]string{padCell("", 2), padCell("NAME", 18), padCell("PORT", 8), padCell("CWD", 32), "COMMAND"}, " ") + "\n")
	}
	for i, name := range m.services {
		svc := m.cfg.Services[name]
		prefix := mutedStyle.Render(" ")
		if i == m.cursor {
			prefix = selectedStyle.Render("▸")
		}
		port := "-"
		if svc.Port > 0 {
			port = strconv.Itoa(svc.Port)
		}
		if compact {
			b.WriteString(prefix + " " + strings.Join([]string{padCell(truncate(name, 18), 18), padCell(port, 8), truncate(svc.Command, 50)}, " ") + "\n")
		} else {
			b.WriteString(prefix + " " + strings.Join([]string{padCell(truncate(name, 18), 18), padCell(port, 8), padCell(truncate(emptyDot(svc.CWD), 32), 32), truncate(svc.Command, 70)}, " ") + "\n")
		}
	}
	return b.String()
}

func (m appModel) renderGroupsView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Groups") + "\n")
	if len(m.groups) == 0 {
		b.WriteString(mutedStyle.Render("No groups yet.") + "\n")
		return b.String()
	}
	for i, name := range m.groups {
		prefix := mutedStyle.Render(" ")
		if i == m.groupCur {
			prefix = selectedStyle.Render("▸")
		}
		b.WriteString(prefix + " " + padCell(name, 18) + strings.Join(m.cfg.Groups[name], ", ") + "\n")
	}
	return b.String()
}

func (m appModel) renderServiceForm() string {
	var b strings.Builder
	title := "Add service"
	if m.form.editName != "" {
		title = "Edit service"
	}
	b.WriteString(headerStyle.Render(title) + mutedStyle.Render("  ctrl+s saves, esc cancels") + "\n")
	b.WriteString(mutedStyle.Render("Fill only what you need. Name and command are required.") + "\n\n")

	fieldWidth := max(34, m.width-18)
	for i := range m.form.inputs {
		m.form.inputs[i].Width = max(20, fieldWidth-2)
		b.WriteString(renderInputBlock(
			serviceFieldLabel(i),
			m.form.inputs[i].View(),
			serviceFieldHint(i),
			serviceFieldRequired(i),
			i == m.form.focus,
			fieldWidth,
		))
		if i < len(m.form.inputs)-1 {
			b.WriteString("\n")
		}
	}
	if m.message != "" {
		b.WriteString("\n" + redStyle.Render(m.message) + "\n")
	}
	body := m.panelWithHeight(b.String(), max(12, m.height-6))
	help := m.panel(strings.Join([]string{
		mutedStyle.Render("tab/down") + " next",
		mutedStyle.Render("shift+tab/up") + " previous",
		mutedStyle.Render("ctrl+s") + " save",
		mutedStyle.Render("esc") + " cancel",
	}, "   "))
	return m.withBottomHelp(body+"\n", help)
}

func renderInputBlock(label, value, hint string, required, focused bool, width int) string {
	var b strings.Builder
	nameStyle := mutedStyle
	boxStyle := inputBoxStyle
	if focused {
		nameStyle = cyanStyle
		boxStyle = inputFocusStyle
	}
	tag := mutedStyle.Render("optional")
	if required {
		tag = yellowStyle.Render("required")
	}
	b.WriteString(nameStyle.Render(label) + " " + tag + "\n")
	b.WriteString(boxStyle.Width(width).Render(value) + "\n")
	if focused && hint != "" {
		b.WriteString(mutedStyle.Render(hint) + "\n")
	}
	return b.String()
}

func serviceFieldLabel(index int) string {
	labels := []string{
		"Service name",
		"Working directory",
		"Command",
		"Local port",
		"Restart on crash",
		"Start at",
		"Repeat every",
	}
	if index < 0 || index >= len(labels) {
		return ""
	}
	return labels[index]
}

func serviceFieldRequired(index int) bool {
	return index == 0 || index == 2
}

func serviceFieldHint(index int) string {
	hints := []string{
		"Unique name used by up, groups, and the monitor.",
		"Folder where the command runs. Leave empty to use the current directory.",
		"Any command you normally type in the terminal.",
		"Optional. Enables localhost link and stronger port cleanup on stop.",
		"true keeps long-running dev services alive; false is better for scheduled jobs.",
		"Optional daily start time in HH:MM.",
		"Optional interval like 30m, 2h, or 1h30m.",
	}
	if index < 0 || index >= len(hints) {
		return ""
	}
	return hints[index]
}

func newGroupForm(editName string, services, selected []string) *groupForm {
	name := textinput.New()
	name.Prompt = ""
	name.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	name.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	name.Placeholder = "morning"
	name.SetValue(editName)
	name.Focus()
	marked := make(map[string]bool)
	for _, svc := range selected {
		marked[svc] = true
	}
	return &groupForm{
		editName:  editName,
		name:      name,
		services:  append([]string(nil), services...),
		marked:    marked,
		nameFocus: true,
	}
}

func (m appModel) updateGroupForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.groupForm.nameFocus && key != "esc" && key != "ctrl+s" && key != "tab" && key != "shift+tab" {
		var cmd tea.Cmd
		m.groupForm.name, cmd = m.groupForm.name.Update(msg)
		return m, cmd
	}
	switch key {
	case "esc":
		m.groupForm = nil
		if m.returnAdd {
			m.returnAdd = false
			m.openAddMenu("")
		} else {
			m.message = ""
		}
		return m, nil
	case "ctrl+s":
		if err := m.saveGroupForm(); err != nil {
			m.message = err.Error()
		} else {
			m.groupForm = nil
			m.refreshConfigLists()
			if m.returnAdd {
				m.returnAdd = false
				m.openAddMenu("group saved")
			} else {
				m.message = "group saved"
			}
		}
		return m, nil
	case "tab":
		m.groupForm.name.Blur()
		m.groupForm.nameFocus = false
		return m, nil
	case "shift+tab":
		m.groupForm.nameFocus = true
		m.groupForm.name.Focus()
		return m, nil
	case "up", "k":
		if m.groupForm.cursor > 0 {
			m.groupForm.cursor--
		}
		return m, nil
	case "down", "j":
		if m.groupForm.cursor < len(m.groupForm.services)-1 {
			m.groupForm.cursor++
		}
		return m, nil
	case " ":
		if !m.groupForm.nameFocus && len(m.groupForm.services) > 0 {
			name := m.groupForm.services[m.groupForm.cursor]
			m.groupForm.marked[name] = !m.groupForm.marked[name]
		}
		return m, nil
	}
	if !m.groupForm.nameFocus {
		return m, nil
	}
	var cmd tea.Cmd
	m.groupForm.name, cmd = m.groupForm.name.Update(msg)
	return m, cmd
}

func (m *appModel) saveGroupForm() error {
	name := strings.TrimSpace(m.groupForm.name.Value())
	if err := validateName(name); err != nil {
		return err
	}
	members := make([]string, 0)
	for _, service := range m.groupForm.services {
		if m.groupForm.marked[service] {
			members = append(members, service)
		}
	}
	if m.groupForm.editName != "" && m.groupForm.editName != name {
		delete(m.cfg.Groups, m.groupForm.editName)
	}
	m.cfg.Groups[name] = members
	return saveConfig(m.cfg)
}

func (m appModel) renderGroupForm() string {
	var b strings.Builder
	title := "Create group"
	if m.groupForm.editName != "" {
		title = "Edit group"
	}
	b.WriteString(headerStyle.Render(title) + mutedStyle.Render("  ctrl+s saves, esc cancels") + "\n")
	b.WriteString(mutedStyle.Render("Groups start multiple services together and skip anything already running.") + "\n\n")

	fieldWidth := max(34, m.width-18)
	m.groupForm.name.Width = max(20, fieldWidth-2)
	b.WriteString(renderInputBlock(
		"Group name",
		m.groupForm.name.View(),
		"Pick a short name you can recognize quickly in the start menu.",
		true,
		m.groupForm.nameFocus,
		fieldWidth,
	))
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("Services") + " " + mutedStyle.Render("space toggles selected services") + "\n")
	maxRows := max(6, m.height-16)
	start := 0
	if len(m.groupForm.services) > maxRows {
		start = m.groupForm.cursor - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > len(m.groupForm.services) {
			start = len(m.groupForm.services) - maxRows
		}
	}
	end := min(len(m.groupForm.services), start+maxRows)
	if start > 0 {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ... %d more above", start)) + "\n")
	}
	for i := start; i < end; i++ {
		service := m.groupForm.services[i]
		mark := "[ ]"
		if m.groupForm.marked[service] {
			mark = "[x]"
		}
		line := "  " + mark + " " + service
		if !m.groupForm.nameFocus && i == m.groupForm.cursor {
			line = selectedStyle.Render("▸ " + mark + " " + service)
		}
		b.WriteString(line + "\n")
	}
	if end < len(m.groupForm.services) {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ... %d more below", len(m.groupForm.services)-end)) + "\n")
	}
	if m.message != "" {
		b.WriteString("\n" + redStyle.Render(m.message) + "\n")
	}
	body := m.panelWithHeight(b.String(), max(10, m.height-6))
	help := m.panel(strings.Join([]string{
		mutedStyle.Render("tab/down") + " next",
		mutedStyle.Render("shift+tab") + " name",
		mutedStyle.Render("space") + " toggle",
		mutedStyle.Render("ctrl+s") + " save",
		mutedStyle.Render("esc") + " cancel",
	}, "   "))
	return m.withBottomHelp(body+"\n", help)
}

func (m appModel) renderTable(stateByName map[string]RuntimeState) string {
	return m.renderTableLimited(stateByName, -1, true)
}

func (m appModel) renderTableLimited(stateByName map[string]RuntimeState, maxRows int, showLogo bool) string {
	compact := m.width < 96
	var b strings.Builder
	b.WriteString(headerStyle.Render("Active services") + "\n")
	if compact {
		b.WriteString(m.renderTableHeader([]int{2, 22, 16, 12, 9}, []string{"", "SERVICE NAME", "STATUS", "LOCAL PORT", "NEXT RUN"}))
	} else {
		b.WriteString(m.renderTableHeader([]int{2, 24, 16, 10, 12, 10, 10, 9}, []string{"", "SERVICE NAME", "STATUS", "PROCESS ID", "LOCAL PORT", "UPTIME", "RESTARTS", "NEXT RUN"}))
	}
	active := m.activeStatesFromMap(stateByName)
	if len(active) == 0 {
		b.WriteString(mutedStyle.Render("No active services. Press 'a' to start one.") + "\n")
		if showLogo {
			return m.withMonitorLogo(b.String())
		}
		return b.String()
	}
	if maxRows >= 0 && m.cursor >= maxRows {
		start := m.cursor - maxRows + 1
		if start < 0 {
			start = 0
		}
		active = active[start:]
	}
	if maxRows >= 0 && len(active) > maxRows {
		active = active[:maxRows]
	}
	for _, state := range active {
		prefix := " "
		if state.Name == m.selectedActiveService() {
			prefix = ">"
		}
		b.WriteString(m.renderStateRow(prefix, state, compact))
	}
	if showLogo {
		return m.withMonitorLogo(b.String())
	}
	return b.String()
}

func (m appModel) renderTableHeader(widths []int, labels []string) string {
	return headerStyle.Render(joinCells(widths, labels)) + "\n" + mutedStyle.Render(tableRule(widths)) + "\n"
}

func (m appModel) renderStateRow(prefix string, state RuntimeState, compact bool) string {
	pid := "-"
	if state.PID > 0 {
		pid = strconv.Itoa(state.PID)
	}
	uptime := "-"
	if !state.StartedAt.IsZero() && state.PID > 0 {
		uptime = shortDuration(time.Since(state.StartedAt))
	}
	next := "-"
	if !state.NextRun.IsZero() {
		next = state.NextRun.Format("15:04:05")
	}

	if compact {
		widths := []int{2, 22, 16, 12, 9}
		values := []string{
			cursorCell(prefix),
			truncate(state.Name, 22),
			styledStatus(state.Status),
			portCell(state.Port, 12),
			next,
		}
		return joinCells(widths, values) + "\n"
	}
	widths := []int{2, 24, 16, 10, 12, 10, 10, 9}
	values := []string{
		cursorCell(prefix),
		truncate(state.Name, 24),
		styledStatus(state.Status),
		pid,
		portCell(state.Port, 12),
		uptime,
		strconv.Itoa(state.Restarts),
		next,
	}
	return joinCells(widths, values) + "\n"
}

func (m appModel) withMonitorLogo(table string) string {
	logoText := strings.Trim(`
   __  ______
  / / / / __ \
 / / / / /_/ /
/ /_/ / ____/
\____/_/
`, "\n")
	logo := logoStyle.Render(logoText)
	contentWidth := max(48, m.width-8)
	tableLines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	logoLines := strings.Split(logo, "\n")
	tableWidth := 0
	for _, line := range tableLines {
		if width := lipgloss.Width(stripOSC(line)); width > tableWidth {
			tableWidth = width
		}
	}
	logoWidth := 0
	for _, line := range logoLines {
		if width := lipgloss.Width(stripOSC(line)); width > logoWidth {
			logoWidth = width
		}
	}
	gap := contentWidth - tableWidth - logoWidth
	if gap < 6 {
		return table
	}
	lines := make([]string, max(len(tableLines), len(logoLines)))
	logoStart := 0
	if len(tableLines) > len(logoLines) {
		logoStart = (len(tableLines) - len(logoLines)) / 2
	}
	for i := range lines {
		left := ""
		if i < len(tableLines) {
			left = tableLines[i]
		}
		right := ""
		logoIndex := i - logoStart
		if logoIndex >= 0 && logoIndex < len(logoLines) {
			right = logoLines[logoIndex]
		}
		lines[i] = padCell(left, tableWidth) + strings.Repeat(" ", gap) + right
	}
	return strings.Join(lines, "\n") + "\n"
}

func cursorCell(value string) string {
	if value == ">" {
		return selectedStyle.Render(">")
	}
	return mutedStyle.Render(" ")
}

func portCell(port, width int) string {
	if port <= 0 {
		return "-"
	}
	label := strconv.Itoa(port)
	padded := padCell(label, width)
	return terminalLink("http://localhost:"+label, padded)
}

func joinCells(widths []int, values []string) string {
	parts := make([]string, 0, len(values))
	for i, value := range values {
		width := 10
		if i < len(widths) {
			width = widths[i]
		}
		parts = append(parts, padCell(value, width))
	}
	return strings.Join(parts, " ")
}

func tableRule(widths []int) string {
	total := 0
	for _, width := range widths {
		total += width
	}
	if len(widths) > 1 {
		total += len(widths) - 1
	}
	return strings.Repeat("─", total)
}

func (m appModel) renderLogs(logs []RuntimeLog) string {
	return m.renderLogsLimited(logs, max(5, m.height-14))
}

func (m appModel) renderLogsContent(logs []RuntimeLog) string {
	return m.renderLogsLimited(logs, max(1, len(logs)))
}

func (m appModel) renderLogsLimited(logs []RuntimeLog, maxRows int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Logs ") + mutedStyle.Render("focused/latest") + "\n")
	if len(logs) == 0 {
		b.WriteString(mutedStyle.Render("No logs yet") + "\n")
		return b.String()
	}
	msgWidth := max(20, min(m.width, 120)-34)
	start := 0
	maxRows = max(1, maxRows)
	if len(logs) > maxRows {
		start = len(logs) - maxRows
	}
	for _, entry := range logs[start:] {
		stream := entry.Stream
		if stream == "err" {
			stream = redStyle.Render(stream)
		} else if stream == "sys" {
			stream = cyanStyle.Render(stream)
		} else {
			stream = mutedStyle.Render(stream)
		}
		b.WriteString(fmt.Sprintf("%s %-16s %-8s %s\n",
			mutedStyle.Render(entry.Time.Format("15:04:05")),
			"["+truncate(entry.Service, 14)+"]",
			stream,
			truncate(entry.Message, msgWidth),
		))
	}
	return b.String()
}

func (m appModel) renderAddPicker() string {
	return m.renderAddPage()
}

func (m appModel) renderAddPage() string {
	choices := m.addItems
	var b strings.Builder
	b.WriteString(headerStyle.Render("Start services") + "\n")
	b.WriteString(mutedStyle.Render("Select services or groups to add to this session. Running services are skipped.") + "\n\n")
	if m.message != "" {
		b.WriteString(cyanStyle.Render(m.message) + "\n\n")
	}
	if len(choices) == 0 {
		b.WriteString(mutedStyle.Render("No stopped services available") + "\n")
		help := m.panel(m.renderAddHelp())
		return m.withBottomHelp(m.panelWithHeight(b.String(), max(10, m.height-6))+"\n", help)
	}
	maxRows := max(6, m.height-11)
	start := 0
	if len(choices) > maxRows {
		start = m.addCursor - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > len(choices) {
			start = len(choices) - maxRows
		}
	}
	end := min(len(choices), start+maxRows)
	if start > 0 {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ... %d more above", start)) + "\n")
	}
	lastKind := ""
	for i := start; i < end; i++ {
		item := choices[i]
		if item.Kind != lastKind {
			if lastKind != "" {
				b.WriteString("\n")
			}
			b.WriteString(headerStyle.Render(strings.ToUpper(item.Kind)+"S") + "\n")
			lastKind = item.Kind
		}
		key := addItemKey(item)
		services := m.servicesForAddItem(item)
		runningCount := 0
		for _, service := range services {
			if m.runner.IsRunning(service) {
				runningCount++
			}
		}
		allRunning := len(services) > 0 && runningCount == len(services)
		mark := "[ ]"
		if allRunning {
			mark = "[•]"
		} else if m.addMarked[key] {
			mark = "[x]"
		}
		b.WriteString(m.renderAddItemRow(item, mark, runningCount, i == m.addCursor) + "\n")
	}
	if end < len(choices) {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ... %d more below", len(choices)-end)) + "\n")
	}
	help := m.panel(m.renderAddHelp())
	return m.withBottomHelp(m.panelWithHeight(b.String(), max(12, m.height-6))+"\n", help)
}

func (m appModel) renderAddItemRow(item addItem, mark string, runningCount int, selected bool) string {
	services := m.servicesForAddItem(item)
	allRunning := len(services) > 0 && runningCount == len(services)
	cursor := mutedStyle.Render(" ")
	if selected {
		cursor = selectedStyle.Render(">")
	}

	nameWidth := 30
	statusWidth := 14
	detailWidth := max(16, m.width-nameWidth-statusWidth-20)
	if m.width < 88 {
		nameWidth = 26
		detailWidth = max(10, m.width-nameWidth-statusWidth-16)
	}

	name := item.Name
	status := mutedStyle.Render("○ stopped")
	detail := ""
	if item.Kind == "group" {
		status = mutedStyle.Render(fmt.Sprintf("%d/%d running", runningCount, len(services)))
		if runningCount == len(services) && len(services) > 0 {
			status = greenStyle.Render(fmt.Sprintf("%d/%d running", runningCount, len(services)))
		}
		detail = truncate(strings.Join(services, ", "), detailWidth)
	} else {
		if allRunning {
			status = greenStyle.Render("● running")
			mark = "   "
		}
		if svc, ok := m.cfg.Services[item.Name]; ok && svc.Port > 0 {
			detail = terminalLink("http://localhost:"+strconv.Itoa(svc.Port), "localhost:"+strconv.Itoa(svc.Port))
		}
	}

	left := cursor + " " + mutedStyle.Render(mark) + " " + padCell(name, nameWidth)
	if selected && !allRunning {
		left = cursor + " " + selectedStyle.Render(mark) + " " + selectedStyle.Render(padCell(name, nameWidth))
	}
	return left + " " + padCell(status, statusWidth) + " " + mutedStyle.Render(detail)
}

func (m appModel) renderAddHelp() string {
	return m.renderHelpGrid([]helpItem{
		{"up/down", "move"},
		{"space", "mark"},
		{"enter", "start"},
		{"n", "new service"},
		{"g", "new group"},
		{"e", "edit"},
		{"esc", "close"},
	})
}

func (m *appModel) openAddMenu(message string) {
	m.addMode = true
	m.addCursor = 0
	m.addMarked = make(map[string]bool)
	m.addItems = m.buildAddItems()
	m.message = message
}

func (m appModel) availableServices() []string {
	out := append([]string(nil), m.services...)
	return out
}

func (m appModel) buildAddItems() []addItem {
	items := make([]addItem, 0, len(m.groups)+len(m.services))
	for _, group := range m.groups {
		items = append(items, addItem{Kind: "group", Name: group})
	}
	for _, service := range m.services {
		items = append(items, addItem{Kind: "service", Name: service})
	}
	return items
}

func addItemKey(item addItem) string {
	return item.Kind + ":" + item.Name
}

func (m appModel) servicesForAddKey(key string) []string {
	kind, name, ok := strings.Cut(key, ":")
	if !ok {
		return nil
	}
	return m.servicesForAddItem(addItem{Kind: kind, Name: name})
}

func (m appModel) servicesForAddItem(item addItem) []string {
	if item.Kind == "group" {
		return append([]string(nil), m.cfg.Groups[item.Name]...)
	}
	return []string{item.Name}
}

func uniqueNotRunning(names []string, runner *Runner) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || seen[name] || runner.IsRunning(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (m appModel) activeNames() []string {
	states, _ := m.mon.snapshot()
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	sort.Strings(names)
	return names
}

func (m appModel) activeStatesFromMap(stateByName map[string]RuntimeState) []RuntimeState {
	states := make([]RuntimeState, 0, len(stateByName))
	for _, state := range stateByName {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states
}

func (m appModel) selectedActiveService() string {
	names := m.activeNames()
	if m.cursor < 0 || m.cursor >= len(names) {
		return ""
	}
	return names[m.cursor]
}

func (m appModel) selectedConfigService() string {
	if m.cursor < 0 || m.cursor >= len(m.services) {
		return ""
	}
	return m.services[m.cursor]
}

func (m appModel) selectedGroup() string {
	if m.groupCur < 0 || m.groupCur >= len(m.groups) {
		return ""
	}
	return m.groups[m.groupCur]
}

func (m appModel) selectedServiceForAction() string {
	if m.tab == "services" {
		return m.selectedConfigService()
	}
	return m.selectedActiveService()
}

func (m *appModel) nextTab() {
	switch m.tab {
	case "services":
		m.tab = "groups"
	case "groups":
		m.tab = "monitor"
	default:
		m.tab = "services"
	}
	m.cursor = 0
}

func (m *appModel) prevTab() {
	switch m.tab {
	case "services":
		m.tab = "monitor"
	case "groups":
		m.tab = "services"
	default:
		m.tab = "groups"
	}
	m.cursor = 0
}

func (m *appModel) refreshConfigLists() {
	m.services = m.services[:0]
	for name := range m.cfg.Services {
		m.services = append(m.services, name)
	}
	sort.Strings(m.services)
	m.groups = m.groups[:0]
	for name := range m.cfg.Groups {
		m.groups = append(m.groups, name)
	}
	sort.Strings(m.groups)
	if m.cursor >= len(m.services) {
		m.cursor = max(0, len(m.services)-1)
	}
	if m.groupCur >= len(m.groups) {
		m.groupCur = max(0, len(m.groups)-1)
	}
}

func emptyDot(value string) string {
	if strings.TrimSpace(value) == "" {
		return "."
	}
	return value
}

func styledStatus(status string) string {
	label := strings.ToUpper(status)
	switch status {
	case "running":
		return greenStyle.Render("● " + label)
	case "starting", "restarting":
		return yellowStyle.Render("◆ " + label)
	case "crashed", "error":
		return redStyle.Render("✕ " + label)
	case "scheduled", "queued":
		return cyanStyle.Render("◷ " + label)
	case "stopping":
		return yellowStyle.Render("■ " + label)
	default:
		return mutedStyle.Render("○ " + label)
	}
}

func padCell(value string, width int) string {
	visible := lipgloss.Width(stripOSC(value))
	if visible >= width {
		return value
	}
	return value + strings.Repeat(" ", width-visible)
}

func stripOSC(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == ']' {
			i += 2
			for i < len(value) {
				if value[i] == '\a' {
					break
				}
				if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '\\' {
					i++
					break
				}
				i++
			}
			continue
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

func (m appModel) panel(content string) string {
	width := max(48, m.width-2)
	return panelStyle.Width(width).Render(strings.TrimRight(content, "\n"))
}

func (m appModel) panelWithHeight(content string, height int) string {
	width := max(48, m.width-2)
	return panelStyle.Width(width).Height(height).Render(strings.TrimRight(content, "\n"))
}

func (m appModel) withBottomHelp(body, help string) string {
	body = strings.TrimRight(body, "\n")
	help = strings.TrimRight(help, "\n")
	used := lipgloss.Height(body) + lipgloss.Height(help)
	gap := m.height - used
	if gap < 0 {
		gap = 0
	}
	return body + strings.Repeat("\n", gap) + help
}

func (m appModel) helpText() string {
	if m.tab == "services" {
		return m.renderHelpGrid([]helpItem{
			{"a", "add"},
			{"e", "edit"},
			{"d", "delete"},
			{"enter/r", "run"},
			{"q", "quit"},
		})
	}
	if m.tab == "groups" {
		return m.renderHelpGrid([]helpItem{
			{"up/down", "move"},
			{"e", "edit"},
			{"r", "run group"},
			{"q", "quit"},
		})
	}
	return m.renderHelpGrid([]helpItem{
		{"up/down", "move"},
		{"a", "add"},
		{"n", "new"},
		{"e", "edit"},
		{"g", "group"},
		{"c", "clear logs"},
		{"s", "stop"},
		{"r", "restart"},
		{"q", "quit"},
	})
}

type helpItem struct {
	Key    string
	Action string
}

func (m appModel) renderHelpGrid(items []helpItem) string {
	if len(items) == 0 {
		return ""
	}
	cellWidth := 18
	if m.width < 80 {
		cellWidth = 16
	}
	usable := max(18, m.width-8)
	columns := max(1, usable/(cellWidth+2))
	if columns > len(items) {
		columns = len(items)
	}

	var b strings.Builder
	for i, item := range items {
		cell := keyStyle.Render(item.Key) + " " + mutedStyle.Render(item.Action)
		b.WriteString(padCell(cell, cellWidth))
		if (i+1)%columns == 0 || i == len(items)-1 {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  ")
	}
	return strings.TrimRight(b.String(), "\n")
}

func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func terminalLink(url, label string) string {
	if os.Getenv("UP_NO_LINKS") == "1" {
		return label
	}
	return "\033]8;;" + url + "\033\\" + label + "\033]8;;\033\\"
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return cmdOpenApp(nil)
	}

	switch args[0] {
	case "add", "a":
		return cmdAdd(args[1:])
	case "list", "ls", "l":
		return cmdList()
	case "delete", "del", "rm", "d":
		return cmdDelete(args[1:])
	case "group", "g":
		if len(args) == 1 {
			return cmdOpenAppAt(nil, "groups")
		}
		return cmdGroup(args[1:])
	case "service", "services":
		return cmdOpenAppAt(nil, "services")
	case "groups":
		return cmdOpenAppAt(nil, "groups")
	case "run", "r":
		return cmdRun(args[1:])
	case "version", "v", "--version", "-v":
		printVersion()
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return cmdRun(args)
	}
}

func printUsage() {
	fmt.Println(`up - local dev command supervisor

Usage:
  up add <name> [--cwd <path>] [--port <port>] [--env KEY=VALUE] [--at HH:MM] [--every 2h] <command>
  up run <name|group|all>[,<name|group>...]
  up list
  up delete <name>
  up group add <group> <service...>
  up group remove <group> <service...>
  up group delete <group>
  up version

Examples:
  up add claude-web --cwd F:\projects\claude-web --port 8766 "uv run python main.py"
  up add front --cwd F:\projects\front --port 5173 "npm run dev"
  up group add morning claude-web front
  up run morning`)
}

func printVersion() {
	version := "dev"
	revision := ""
	modified := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if len(setting.Value) >= 12 {
					revision = setting.Value[:12]
				} else {
					revision = setting.Value
				}
			case "vcs.modified":
				modified = setting.Value
			}
		}
	}
	fmt.Printf("up %s\n", version)
	if revision != "" {
		fmt.Printf("commit %s\n", revision)
	}
	if modified == "true" {
		fmt.Println("modified true")
	}
}

func cmdAdd(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: up add <name> [--cwd <path>] [--port <port>] <command>")
	}

	name := args[0]
	if err := validateName(name); err != nil {
		return err
	}

	cwd := ""
	port := 0
	restart := true
	env := envFlags{}
	schedule := Schedule{}
	i := 1
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "--") {
			break
		}

		switch {
		case arg == "--cwd":
			i++
			if i >= len(args) {
				return errors.New("--cwd requires a value")
			}
			cwd = args[i]
		case strings.HasPrefix(arg, "--cwd="):
			cwd = strings.TrimPrefix(arg, "--cwd=")
		case arg == "--port":
			i++
			if i >= len(args) {
				return errors.New("--port requires a value")
			}
			parsed, err := strconv.Atoi(args[i])
			if err != nil || parsed <= 0 {
				return errors.New("--port must be a positive number")
			}
			port = parsed
		case strings.HasPrefix(arg, "--port="):
			parsed, err := strconv.Atoi(strings.TrimPrefix(arg, "--port="))
			if err != nil || parsed <= 0 {
				return errors.New("--port must be a positive number")
			}
			port = parsed
		case arg == "--env":
			i++
			if i >= len(args) {
				return errors.New("--env requires KEY=VALUE")
			}
			if err := env.Set(args[i]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "--env="):
			if err := env.Set(strings.TrimPrefix(arg, "--env=")); err != nil {
				return err
			}
		case arg == "--at":
			i++
			if i >= len(args) {
				return errors.New("--at requires HH:MM")
			}
			schedule.At = args[i]
		case strings.HasPrefix(arg, "--at="):
			schedule.At = strings.TrimPrefix(arg, "--at=")
		case arg == "--every":
			i++
			if i >= len(args) {
				return errors.New("--every requires a duration like 2h or 30m")
			}
			schedule.Every = args[i]
		case strings.HasPrefix(arg, "--every="):
			schedule.Every = strings.TrimPrefix(arg, "--every=")
		case arg == "--restart":
			restart = true
		case arg == "--no-restart":
			restart = false
		case strings.HasPrefix(arg, "--restart="):
			parsed, err := strconv.ParseBool(strings.TrimPrefix(arg, "--restart="))
			if err != nil {
				return errors.New("--restart must be true or false")
			}
			restart = parsed
		default:
			return fmt.Errorf("unknown add option %q", arg)
		}
		i++
	}

	if i >= len(args) {
		return errors.New("command is required")
	}
	command := strings.Join(args[i:], " ")
	if strings.TrimSpace(command) == "" {
		return errors.New("command is required")
	}
	if err := validateSchedule(schedule); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Services[name] = Service{
		CWD:      strings.TrimSpace(cwd),
		Command:  command,
		Port:     port,
		Env:      env.values,
		Restart:  restart,
		Schedule: schedule,
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}

	fmt.Printf("Service %q saved\n", name)
	return nil
}

func cmdList() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Services) == 0 {
		fmt.Println("No services saved")
		return nil
	}

	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Printf("%-18s %-7s %-14s %-32s %s\n", "NAME", "PORT", "SCHEDULE", "CWD", "COMMAND")
	for _, name := range names {
		s := cfg.Services[name]
		port := "-"
		if s.Port > 0 {
			port = strconv.Itoa(s.Port)
		}
		schedule := scheduleLabel(s.Schedule)
		cwd := s.CWD
		if cwd == "" {
			cwd = "."
		}
		fmt.Printf("%-18s %-7s %-14s %-32s %s\n", name, port, schedule, truncate(cwd, 32), s.Command)
	}

	if len(cfg.Groups) > 0 {
		fmt.Println()
		groupNames := make([]string, 0, len(cfg.Groups))
		for name := range cfg.Groups {
			groupNames = append(groupNames, name)
		}
		sort.Strings(groupNames)
		fmt.Println("Groups:")
		for _, name := range groupNames {
			fmt.Printf("  %-16s %s\n", name, strings.Join(cfg.Groups[name], ", "))
		}
	}
	return nil
}

func cmdDelete(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: up delete <name>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	name := args[0]
	if _, ok := cfg.Services[name]; !ok {
		return fmt.Errorf("service %q not found", name)
	}
	delete(cfg.Services, name)
	for group, members := range cfg.Groups {
		cfg.Groups[group] = without(members, name)
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Service %q deleted\n", name)
	return nil
}

func cmdGroup(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: up group <add|remove|delete|list> ...")
	}
	switch args[0] {
	case "list", "ls", "l":
		return cmdList()
	case "add", "a":
		if len(args) < 3 {
			return errors.New("usage: up group add <group> <service...>")
		}
		return updateGroup(args[1], args[2:], true)
	case "remove", "rm", "r":
		if len(args) < 3 {
			return errors.New("usage: up group remove <group> <service...>")
		}
		return updateGroup(args[1], args[2:], false)
	case "delete", "del", "d":
		if len(args) != 2 {
			return errors.New("usage: up group delete <group>")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		delete(cfg.Groups, args[1])
		return saveConfig(cfg)
	default:
		return fmt.Errorf("unknown group command %q", args[0])
	}
}

func updateGroup(group string, services []string, add bool) error {
	if err := validateName(group); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, svc := range services {
		if _, ok := cfg.Services[svc]; !ok {
			return fmt.Errorf("service %q not found", svc)
		}
	}
	current := cfg.Groups[group]
	if add {
		seen := make(map[string]bool, len(current))
		for _, svc := range current {
			seen[svc] = true
		}
		for _, svc := range services {
			if !seen[svc] {
				current = append(current, svc)
			}
		}
		cfg.Groups[group] = current
	} else {
		for _, svc := range services {
			current = without(current, svc)
		}
		cfg.Groups[group] = current
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Group %q updated\n", group)
	return nil
}

func cmdRun(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: up run <name|group|all>[,<name|group>...]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names, err := resolveTargets(cfg, args[0])
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("nothing to run")
	}
	return cmdOpenApp(names)
}

func cmdOpenApp(names []string) error {
	return cmdOpenAppAt(names, "")
}

func cmdOpenAppAt(names []string, tab string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mon := newMonitor(names, cfg)
	runner := newRunner(ctx, cfg, mon)
	model := newAppModel(cfg, mon, runner, names)
	if tab != "" {
		model.tab = tab
	}
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithoutSignalHandler())
	go func() {
		<-ctx.Done()
		runner.StopAll()
		program.Quit()
	}()
	_, err = program.Run()
	runner.StopAll()
	cancel()
	return err
}

func supervise(ctx context.Context, mon *Monitor, name string, svc Service) {
	if svc.Schedule.At != "" || svc.Schedule.Every != "" {
		superviseScheduled(ctx, mon, name, svc)
		return
	}
	superviseNow(ctx, mon, name, svc)
}

func superviseScheduled(ctx context.Context, mon *Monitor, name string, svc Service) {
	for {
		next, err := nextScheduledRun(svc.Schedule, time.Now())
		if err != nil {
			mon.update(name, func(state *RuntimeState) {
				state.Status = "error"
				state.LastExit = err.Error()
			})
			return
		}
		mon.update(name, func(state *RuntimeState) {
			state.Status = "scheduled"
			state.NextRun = next
		})
		mon.addLog(name, "sys", "scheduled for "+next.Format("2006-01-02 15:04:05"))

		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if svc.Restart {
			superviseNow(ctx, mon, name, svc)
			return
		}

		_, _ = runOnce(ctx, mon, name, svc)
		if svc.Schedule.Every == "" {
			mon.update(name, func(state *RuntimeState) {
				state.Status = "stopped"
				state.NextRun = time.Time{}
			})
			return
		}
	}
}

func superviseNow(ctx context.Context, mon *Monitor, name string, svc Service) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		attempt++
		status := "starting"
		if attempt > 1 {
			status = "restarting"
		}
		mon.update(name, func(state *RuntimeState) {
			state.Status = status
			state.NextRun = time.Time{}
			if attempt > 1 {
				state.Restarts = attempt - 1
			}
		})
		mon.addLog(name, "sys", status)

		exitCode, err := runOnce(ctx, mon, name, svc)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			mon.update(name, func(state *RuntimeState) {
				state.Status = "crashed"
				state.PID = 0
				state.LastExit = fmt.Sprintf("exit=%d error=%v", exitCode, err)
			})
			mon.addLog(name, "sys", fmt.Sprintf("crashed exit=%d error=%v", exitCode, err))
		} else {
			mon.update(name, func(state *RuntimeState) {
				state.Status = "stopped"
				state.PID = 0
				state.LastExit = fmt.Sprintf("exit=%d", exitCode)
			})
			mon.addLog(name, "sys", fmt.Sprintf("stopped exit=%d", exitCode))
		}
		if !svc.Restart {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func runOnce(ctx context.Context, mon *Monitor, name string, svc Service) (int, error) {
	cmd := shellCommand(ctx, svc.Command)
	if svc.CWD != "" {
		cmd.Dir = svc.CWD
	}
	cmd.Env = os.Environ()
	for key, value := range svc.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		mon.update(name, func(state *RuntimeState) {
			state.Status = "error"
			state.LastExit = err.Error()
		})
		return -1, err
	}
	go func() {
		<-ctx.Done()
		killProcessTree(cmd.Process.Pid)
		mon.addLog(name, "sys", fmt.Sprintf("killed process tree pid=%d", cmd.Process.Pid))
		if svc.Port > 0 {
			time.Sleep(500 * time.Millisecond)
			pids := killListenersOnPort(svc.Port)
			if len(pids) > 0 {
				mon.addLog(name, "sys", fmt.Sprintf("killed listener(s) on port %d: %v", svc.Port, pids))
			} else {
				mon.addLog(name, "sys", fmt.Sprintf("port %d released", svc.Port))
			}
		}
	}()

	mon.update(name, func(state *RuntimeState) {
		state.Status = "running"
		state.PID = cmd.Process.Pid
		state.Port = svc.Port
		state.StartedAt = time.Now()
		state.LastExit = ""
	})
	mon.addLog(name, "sys", fmt.Sprintf("running pid=%d", cmd.Process.Pid))

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(&wg, mon, name, stdout, false)
	go streamLines(&wg, mon, name, stderr, true)

	err = cmd.Wait()
	wg.Wait()
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), err
	}
	return -1, err
}

func streamLines(wg *sync.WaitGroup, mon *Monitor, name string, r io.Reader, isErr bool) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		prefix := "out"
		if isErr {
			prefix = "err"
		}
		mon.addLog(name, prefix, scanner.Text())
	}
}

func resolveTargets(cfg Config, target string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(name string) error {
		if seen[name] {
			return nil
		}
		if _, ok := cfg.Services[name]; !ok {
			return fmt.Errorf("service %q not found", name)
		}
		seen[name] = true
		out = append(out, name)
		return nil
	}

	for _, part := range strings.Split(target, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "all" {
			names := make([]string, 0, len(cfg.Services))
			for name := range cfg.Services {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if err := add(name); err != nil {
					return nil, err
				}
			}
			continue
		}
		if members, ok := cfg.Groups[part]; ok {
			for _, name := range members {
				if err := add(name); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := add(part); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func loadConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Services: map[string]Service{},
		Groups:   map[string][]string{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Services == nil {
		cfg.Services = map[string]Service{}
	}
	if cfg.Groups == nil {
		cfg.Groups = map[string][]string{}
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "."+appName, "services.json"), nil
}

func validateName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return fmt.Errorf("invalid name %q; use letters, numbers, dash, underscore", name)
		}
	}
	return nil
}

func validateSchedule(schedule Schedule) error {
	if schedule.At != "" {
		if _, err := parseClock(schedule.At, time.Now()); err != nil {
			return fmt.Errorf("--at: %w", err)
		}
	}
	if schedule.Every != "" {
		if _, err := time.ParseDuration(schedule.Every); err != nil {
			return fmt.Errorf("--every: %w", err)
		}
	}
	return nil
}

func scheduleLabel(schedule Schedule) string {
	switch {
	case schedule.At != "" && schedule.Every != "":
		return schedule.At + "/" + schedule.Every
	case schedule.At != "":
		return "at " + schedule.At
	case schedule.Every != "":
		return "every " + schedule.Every
	default:
		return "-"
	}
}

func parseClock(value string, now time.Time) (time.Time, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return time.Time{}, errors.New("expected HH:MM")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, errors.New("hour must be 00..23")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return time.Time{}, errors.New("minute must be 00..59")
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next, nil
}

func nextScheduledRun(schedule Schedule, now time.Time) (time.Time, error) {
	var candidates []time.Time
	if schedule.At != "" {
		next, err := parseClock(schedule.At, now)
		if err != nil {
			return time.Time{}, err
		}
		candidates = append(candidates, next)
	}
	if schedule.Every != "" {
		duration, err := time.ParseDuration(schedule.Every)
		if err != nil {
			return time.Time{}, err
		}
		if duration <= 0 {
			return time.Time{}, errors.New("duration must be positive")
		}
		candidates = append(candidates, now.Add(duration))
	}
	if len(candidates) == 0 {
		return now, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })
	return candidates[0], nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func without(values []string, remove string) []string {
	out := values[:0]
	for _, value := range values {
		if value != remove {
			out = append(out, value)
		}
	}
	return out
}

type envFlags struct {
	values map[string]string
}

func (e *envFlags) String() string {
	if e == nil || len(e.values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e.values))
	for key := range e.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+e.values[key])
	}
	return strings.Join(pairs, ",")
}

func (e *envFlags) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return errors.New("env must be KEY=VALUE")
	}
	if e.values == nil {
		e.values = make(map[string]string)
	}
	e.values[strings.TrimSpace(key)] = val
	return nil
}
