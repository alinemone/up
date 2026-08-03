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

	"golang.org/x/term"
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

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiCyan   = "\033[36m"
	ansiBlue   = "\033[34m"
	ansiGray   = "\033[90m"
)

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

func (m *Monitor) renderLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	m.render()
	for {
		select {
		case <-ctx.Done():
			m.render()
			return
		case <-ticker.C:
			m.render()
		}
	}
}

func (m *Monitor) render() {
	width := terminalWidth()
	compact := width < 86
	ruleWidth := width
	if ruleWidth > 120 {
		ruleWidth = 120
	}
	if ruleWidth < 48 {
		ruleWidth = 48
	}

	m.mu.Lock()
	states := make([]RuntimeState, 0, len(m.states))
	for _, state := range m.states {
		states = append(states, state)
	}
	logs := append([]RuntimeLog(nil), m.logs...)
	m.mu.Unlock()

	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })

	fmt.Print("\033[2J\033[H")
	fmt.Println(ansiBold + ansiCyan + "up" + ansiReset + ansiDim + " local dev monitor" + ansiReset)
	fmt.Println(ansiGray + strings.Repeat("─", ruleWidth) + ansiReset)
	if compact {
		fmt.Printf("%-16s %-13s %-8s %s\n", "SERVICE", "STATUS", "PORT", "NEXT")
	} else {
		fmt.Printf("%-18s %-13s %-7s %-8s %-10s %-9s %s\n", "SERVICE", "STATUS", "PID", "PORT", "UPTIME", "RESTARTS", "NEXT")
	}
	fmt.Println(ansiGray + strings.Repeat("─", ruleWidth) + ansiReset)
	for _, state := range states {
		pid := "-"
		if state.PID > 0 {
			pid = strconv.Itoa(state.PID)
		}
		portText := "-"
		portVisible := 1
		if state.Port > 0 {
			portText = terminalLink("http://localhost:"+strconv.Itoa(state.Port), strconv.Itoa(state.Port))
			portVisible = len(strconv.Itoa(state.Port))
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
			fmt.Printf("%s %s %s %s\n",
				cell(truncate(state.Name, 16), 16),
				colorStatus(state.Status),
				ansiCell(portText, portVisible, 8),
				next,
			)
		} else {
			fmt.Printf("%s %s %s %s %s %s %s\n",
				cell(truncate(state.Name, 18), 18),
				colorStatus(state.Status),
				cell(pid, 7),
				ansiCell(portText, portVisible, 8),
				cell(uptime, 10),
				cell(strconv.Itoa(state.Restarts), 9),
				next,
			)
		}
		if state.LastExit != "" {
			fmt.Println("  " + ansiDim + truncate(state.LastExit, ruleWidth-4) + ansiReset)
		}
	}
	fmt.Println()
	fmt.Println(ansiBold + "Logs" + ansiReset + ansiDim + " (latest)" + ansiReset)
	fmt.Println(ansiGray + strings.Repeat("─", ruleWidth) + ansiReset)
	if len(logs) == 0 {
		fmt.Println(ansiDim + "No logs yet" + ansiReset)
	} else {
		msgWidth := ruleWidth - 30
		if msgWidth < 20 {
			msgWidth = 20
		}
		for _, entry := range logs {
			stream := entry.Stream
			if stream == "err" {
				stream = ansiRed + stream + ansiReset
			} else {
				stream = ansiBlue + stream + ansiReset
			}
			fmt.Printf("%s %-16s %-12s %s\n",
				ansiGray+entry.Time.Format("15:04:05")+ansiReset,
				"["+truncate(entry.Service, 14)+"]",
				stream,
				truncate(entry.Message, msgWidth),
			)
		}
	}
	fmt.Println()
	fmt.Println(ansiDim + "Ctrl+C stops all running services" + ansiReset)
}

func colorStatus(status string) string {
	color := ansiGray
	switch status {
	case "running":
		color = ansiGreen
	case "starting", "restarting":
		color = ansiYellow
	case "crashed", "error":
		color = ansiRed
	case "scheduled", "queued":
		color = ansiCyan
	case "stopped":
		color = ansiDim
	}
	return color + fmt.Sprintf("%-13s", strings.ToUpper(status)) + ansiReset
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

func terminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && width > 0 {
		return width
	}
	if value := strings.TrimSpace(os.Getenv("COLUMNS")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 100
}

func terminalLink(url, label string) string {
	if os.Getenv("UP_NO_LINKS") == "1" {
		return label
	}
	return "\033]8;;" + url + "\033\\" + label + "\033]8;;\033\\"
}

func cell(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func ansiCell(value string, visibleWidth, width int) string {
	if visibleWidth >= width {
		return value
	}
	return value + strings.Repeat(" ", width-visibleWidth)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "add", "a":
		return cmdAdd(args[1:])
	case "list", "ls", "l":
		return cmdList()
	case "delete", "del", "rm", "d":
		return cmdDelete(args[1:])
	case "group", "g":
		return cmdGroup(args[1:])
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mon := newMonitor(names, cfg)
	go mon.renderLoop(ctx)

	var wg sync.WaitGroup
	for _, name := range names {
		svc := cfg.Services[name]
		wg.Add(1)
		go func(name string, svc Service) {
			defer wg.Done()
			supervise(ctx, mon, name, svc)
		}(name, svc)
	}
	wg.Wait()
	mon.render()
	return nil
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

func logLine(name, msg string) {
	fmt.Printf("%s %-16s %s\n", time.Now().Format("15:04:05"), "["+name+"]", msg)
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
