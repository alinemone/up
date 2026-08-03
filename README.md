# up

`up` is a small local development command supervisor. It runs any long-lived
command, streams logs, restarts crashed processes, and shows optional metadata
such as the localhost port.

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
up add <name> [--cwd <path>] [--port <port>] [--env KEY=VALUE] <command>
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
      "restart": true
    }
  },
  "groups": {
    "morning": ["claude-web"]
  }
}
```

## Scheduling Later

The config can later grow a `schedule` field per service without changing the
runtime model:

```json
{
  "schedule": {
    "at": "09:00",
    "every": "2h"
  }
}
```

The first version does not implement scheduling yet.
