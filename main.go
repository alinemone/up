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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const appName = "up"

type Config struct {
	Services map[string]Service  `json:"services"`
	Groups   map[string][]string `json:"groups"`
}

type Service struct {
	CWD     string            `json:"cwd,omitempty"`
	Command string            `json:"command"`
	Port    int               `json:"port,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Restart bool              `json:"restart"`
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
  up add <name> [--cwd <path>] [--port <port>] [--env KEY=VALUE] <command>
  up run <name|group|all>[,<name|group>...]
  up list
  up delete <name>
  up group add <group> <service...>
  up group remove <group> <service...>
  up group delete <group>

Examples:
  up add claude-web --cwd F:\projects\claude-web --port 8766 "uv run python main.py"
  up add front --cwd F:\projects\front --port 5173 "npm run dev"
  up group add morning claude-web front
  up run morning`)
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

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Services[name] = Service{
		CWD:     strings.TrimSpace(cwd),
		Command: command,
		Port:    port,
		Env:     env.values,
		Restart: restart,
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

	fmt.Printf("%-18s %-7s %-32s %s\n", "NAME", "PORT", "CWD", "COMMAND")
	for _, name := range names {
		s := cfg.Services[name]
		port := "-"
		if s.Port > 0 {
			port = strconv.Itoa(s.Port)
		}
		cwd := s.CWD
		if cwd == "" {
			cwd = "."
		}
		fmt.Printf("%-18s %-7s %-32s %s\n", name, port, truncate(cwd, 32), s.Command)
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

	fmt.Printf("Running %s. Press Ctrl+C to stop.\n", strings.Join(names, ", "))

	var wg sync.WaitGroup
	for _, name := range names {
		svc := cfg.Services[name]
		wg.Add(1)
		go func(name string, svc Service) {
			defer wg.Done()
			supervise(ctx, name, svc)
		}(name, svc)
	}
	wg.Wait()
	return nil
}

func supervise(ctx context.Context, name string, svc Service) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		attempt++
		status := "starting"
		if attempt > 1 {
			status = fmt.Sprintf("restarting attempt=%d", attempt)
		}
		logLine(name, status)

		exitCode, err := runOnce(ctx, name, svc)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logLine(name, fmt.Sprintf("crashed exit=%d error=%v", exitCode, err))
		} else {
			logLine(name, fmt.Sprintf("stopped exit=%d", exitCode))
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

func runOnce(ctx context.Context, name string, svc Service) (int, error) {
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
		return -1, err
	}
	go func() {
		<-ctx.Done()
		killProcessTree(cmd.Process.Pid)
	}()

	port := ""
	if svc.Port > 0 {
		port = fmt.Sprintf(" localhost:%d", svc.Port)
	}
	logLine(name, fmt.Sprintf("running pid=%d%s", cmd.Process.Pid, port))

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(&wg, name, stdout, false)
	go streamLines(&wg, name, stderr, true)

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

func streamLines(wg *sync.WaitGroup, name string, r io.Reader, isErr bool) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		prefix := "out"
		if isErr {
			prefix = "err"
		}
		logLine(name, prefix+" "+scanner.Text())
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
