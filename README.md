# up

`up` is a small local development command supervisor. It runs any long-lived
command, shows a colored terminal monitor, streams logs, restarts crashed
processes, supports simple schedules, and shows optional metadata such as the
localhost port.

It is intentionally separate from `pf`: no kubectl, no port-forward assumptions,
and no required health check.

## Build

Install globally with Go:

```powershell
go install github.com/alinemone/up@latest
```

Make sure Go's bin directory is in your `PATH`:

```powershell
go env GOPATH
```

The executable is installed under:

```text
<GOPATH>\bin\up.exe
```

Build locally:

```powershell
go build -o up.exe .
```

## Examples

```powershell
up add claude-web --cwd F:\projects\claude-web --port 8766 --env PYTHONUTF8=1 --env PYTHONIOENCODING=utf-8 "uv run python main.py"
up add front --cwd F:\projects\front --port 5173 "npm run dev"
up add backup-job --every 2h --no-restart "powershell -File F:\jobs\backup.ps1"
up add morning-api --at 09:00 --cwd F:\projects\api --port 8080 "npm run dev"

up group add morning claude-web front
up run morning
```

You can also run a batch file:

```powershell
up add claude-web --port 8766 "call F:\projects\claude-web\dev.bat"
up claude-web
```

## Commands

```text
up add <name> [--cwd <path>] [--port <port>] [--env KEY=VALUE] [--at HH:MM] [--every 2h] <command>
up run <name|group|all>[,<name|group>...]
up list
up delete <name>
up group add <group> <service...>
up group remove <group> <service...>
up group delete <group>
```

## Config

Config is stored at:

```text
~/.up/services.json
```

Shape:

```json
{
  "services": {
    "claude-web": {
      "cwd": "F:\\projects\\claude-web",
      "command": "uv run python main.py",
      "port": 8766,
      "env": {
        "PYTHONUTF8": "1",
        "PYTHONIOENCODING": "utf-8"
      },
      "restart": true,
      "schedule": {
        "at": "09:00",
        "every": "2h"
      }
    }
  },
  "groups": {
    "morning": ["claude-web"]
  }
}
```

## Scheduling

`schedule.at` runs the service at the next daily `HH:MM`.
`schedule.every` runs it after the given Go duration (`30m`, `2h`, `1h30m`).

If a scheduled service has `restart: true`, it is kept alive after its scheduled
start. If it has `restart: false`, `every` behaves like a repeated job interval.

## Monitor

`up run ...` opens a colored terminal dashboard with service status, PID, port,
uptime, restart count, next scheduled run, and recent logs.
