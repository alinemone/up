# up

Bring your local development stack up with one command.

`up` is a terminal-based supervisor for local developer tools and services. It
starts the commands you normally run by hand, keeps long-running processes alive,
shows a responsive colored monitor, streams recent logs, and can start services
on a simple schedule.

Use it for anything you run during development:

- frontend dev servers
- backend APIs
- Python or Go workers
- batch files and shell scripts
- Docker Compose commands
- recurring local jobs

No health endpoint is required. A port is optional metadata used for display and
clickable localhost links.

## Install

```bash
go install github.com/alinemone/up@latest
```

Make sure Go's bin directory is in your `PATH`.

Linux/macOS:

```bash
echo 'export PATH="$PATH:$HOME/go/bin"' >> ~/.bashrc
source ~/.bashrc
```

Windows PowerShell:

```powershell
$env:Path += ";$(go env GOPATH)\bin"
```

Check the installed version:

```bash
up version
```

## Quick Start

Add a service:

```bash
up add api --cwd ~/projects/api --port 8080 "npm run dev"
```

Run it:

```bash
up api
```

Add a few services and run them together:

```bash
up add web --cwd ~/projects/web --port 5173 "npm run dev"
up add worker --cwd ~/projects/worker "go run ./cmd/worker"

up group add dev api web worker
up run dev
```

## Windows Examples

```powershell
up add claude-web --cwd F:\projects\claude-web --port 8766 --env PYTHONUTF8=1 --env PYTHONIOENCODING=utf-8 "uv run python main.py"

up add front --cwd F:\projects\front --port 5173 "npm run dev"

up add claude-web-bat --port 8766 "call F:\projects\claude-web\dev.bat"
```

## Monitor

`up run ...` opens a responsive terminal dashboard:

```text
SERVICE            STATUS        PID     PORT     UPTIME     RESTARTS  NEXT
api                RUNNING       18420   8080     12m04s     0         -
web                RUNNING       18712   5173     11m51s     1         -
backup             SCHEDULED     -       -        -          0         14:00:00
```

The monitor shows:

- current status
- process ID
- optional localhost port
- uptime
- restart count
- next scheduled run
- recent logs
- runtime controls for starting and stopping services

Keys:

- `a` opens the service picker and starts another configured service.
- `enter` starts or stops the selected service.
- `s` stops the selected service.
- `r` restarts the selected service.
- `q` quits and stops all running services.

If a service has a port, terminals that support OSC 8 hyperlinks can open
`http://localhost:<port>` directly from the dashboard.

Disable terminal links:

```bash
UP_NO_LINKS=1 up run all
```

## Scheduling

Start a service at a daily time:

```bash
up add morning-api --at 09:00 --cwd ~/projects/api --port 8080 "npm run dev"
```

Run a repeated job:

```bash
up add backup --every 2h --no-restart "bash ~/jobs/backup.sh"
```

Schedule options:

- `--at HH:MM` starts at the next daily time.
- `--every 30m`, `--every 2h`, or `--every 1h30m` starts after that interval.
- With `--restart` enabled, the process is kept alive after it starts.
- With `--no-restart`, `--every` behaves like a repeated job interval.

## Commands

```text
up add <name> [--cwd <path>] [--port <port>] [--env KEY=VALUE] [--at HH:MM] [--every 2h] <command>
up run <name|group|all>[,<name|group>...]
up list
up delete <name>
up group add <group> <service...>
up group remove <group> <service...>
up group delete <group>
up version
```

Shortcuts:

```bash
up api
up api,web
up all
```

## Configuration

Config is stored at:

```text
~/.up/services.json
```

Example:

```json
{
  "services": {
    "api": {
      "cwd": "/home/ali/projects/api",
      "command": "npm run dev",
      "port": 8080,
      "restart": true
    },
    "backup": {
      "command": "bash /home/ali/jobs/backup.sh",
      "restart": false,
      "schedule": {
        "every": "2h"
      }
    }
  },
  "groups": {
    "dev": ["api"]
  }
}
```
