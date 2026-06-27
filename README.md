# igradle

Interactive Gradle task launcher with multi-module support, live streaming output, and per-task progress.
A single static binary, ported from the fish shell function of the same name.

```
── :backend:auth:test ──────────────────────────────────────
> Task :backend:auth:test
  ✓ CompileJava — completed, 47 source files
  ⋯ TestWorker — running suite #3 of 12 ...
─────────────────────────────────────────────────────────────
 ⠋ 2/3   ✔ 1   ✘ 0      PgUp/PgDn scroll   q skip remaining
```

## What's new in v2

| v1 (fish)                 | v2 (Go)                                          |
| ------------------------- | ------------------------------------------------ |
| Select and exit           | Select, stream output, watch tasks build         |
| `fzf -m`                  | Bubble Tea list + custom delegate                |
| One-shot execution        | Sequential **or** parallel, user choice          |
| Output after exit         | Live stdout/stderr into a scrolling viewport     |
| No progress feedback      | Per-task spinner + elapsed time + pass/fail      |
| Stop on failure           | Stop **or** continue-on-failure, user choice     |
| Single cache format       | Same; cache reused as fast-path                  |

## Features

- **Multi-select** — pick one or many tasks
- **Multi-module aware** — tasks tagged with module path
- **Smart cache** — reuses `gradle tasks --all` until a build file changes
- **Auto-finds `gradlew`** — walks up the directory tree
- **Live streaming** — watch each task build, test, or publish in real time
- **Per-task progress** — spinner while running, ✔ green / ✘ red at the end
- **Two execution modes** — sequential (one at a time, full log) or parallel (all at once)
- **Failure handling** — stop after first failure (with retry/skip) or continue and summarize
- **Cross-platform** — Linux, macOS, Windows; amd64 and arm64
- **Single binary** — no fzf, no awk, no shell, no runtime

## Install

```bash
git clone <this-repo> igradle
cd igradle
make install       # → $GOBIN/igradle (or ~/go/bin/igradle)
```

Cross-compile for all platforms:

```bash
make build-all
ls bin/
# igradle-linux-amd64   igradle-linux-arm64
# igradle-darwin-amd64  igradle-darwin-arm64
# igradle-windows-amd64.exe  igradle-windows-arm64.exe
```

## Usage

```bash
igradle                       # launch selector, ask mode + failure at runtime
igradle -r                    # force-refresh task cache
igradle -n                    # dry run — print commands, don't execute
igradle build test            # extra args forwarded to gradle
igradle --mode parallel       # skip the mode picker
igradle --on-failure continue # don't stop on first failure
igradle -h                    # help
```

### Flags

| Flag                          | Description                                    |
| ----------------------------- | ---------------------------------------------- |
| `-r`, `--refresh`             | Force refresh task cache                       |
| `-n`, `--dry-run`             | Print commands, don't execute                  |
| `-h`, `--help`                | Show help                                      |
| `--mode sequential\|parallel` | Execution mode (skip the picker)               |
| `--on-failure stop\|continue` | Failure handling (skip the picker)             |

### Selector controls

| Key             | Action                  |
| --------------- | ----------------------- |
| `↑` / `↓`       | Move cursor             |
| `j` / `k`       | Move cursor (vim)       |
| `/`             | Filter list             |
| `space`         | Toggle selection        |
| `enter`         | Confirm                 |
| `ctrl-c` / `q`  | Quit                    |

### Mode picker (only when `--mode`/`--on-failure` aren't passed)

```
Run how?
  [s]equential   one at a time, full-screen log per task
  [p]arallel     all at once, combined output

On failure?
  [s]top         stop after first failure
  [c]ontinue    run all tasks, summarize at end
```

### Running controls

| Key             | Action                                                  |
| --------------- | ------------------------------------------------------- |
| `PgUp` / `PgDn` | Scroll log                                              |
| `q`             | Stop running (sequential: skip remaining; parallel: stop) |

When `stop` is chosen and a task fails, you get:

```
── :backend:auth:test FAILED after 00:42 ──
  > Task :backend:auth:test FAILED
  ...
─────────────────────────────────────────────
 1/3   ✔ 1   ✘ 1     [s]kip  [r]etry  [q]uit
```

## Architecture (v2)

```
main.go                       ~ 750 lines, 10 sections
  1. Arg parsing              — flag.NewFlagSet + --mode/--on-failure
  2. Gradle discovery         — gradlew/gradlew.bat lookup
  3. Cache management         — .gradle/igradle_cache.txt, staleness check
  4. Task parsing             — group regex + module path splitting
  5. Ring buffer              — 5000-line drop-oldest log buffer
  6. Streaming subprocess     — bounded chan + tea.Program.Send
  7. Bubble Tea model         — 8 model states, per-task state
  8. View                     — header + viewport + status bar
  9. List delegate            — selector rendering
 10. Main                     — wire it all together
Makefile                      — build, build-all (6 platforms), install
go.mod                        — bubbles, bubbletea, lipgloss
```

### Streaming pattern

`runOneTask` returns `<-chan tea.Msg`. Two reader goroutines (stdout + stderr) feed a **1024-buffer channel** with drop-on-full. The first message is returned inline as the `tea.Cmd`'s value. A long-lived goroutine then drains the rest and calls `programRef.Send` for each line. The TUI collects them in a `ringBuffer` (cap 5000) and coalesces viewport repaints at 60Hz via a `tea.Tick(16ms)`.

```
gradle ──┬─stdout──► scanner ──┐
         │                     ├─► chan tea.Msg (cap 1024, drop-full) ─► programRef.Send ─► Update
         └─stderr──► scanner ──┘                                                   └─► ringBuffer.push
                                                                                     └─► 60Hz flushTick → viewport repaint
```

### Model states

```
stateLoading ─► stateReady ─► stateConfirmCount (≥5 tasks)
                            └► stateModePicker ─► stateRunning ─► stateSummary
                                                  └► stateFailed ─► stateRunning (retry/skip) ─► stateSummary
```

### Per-task status

`taskRun` carries its own spinner and `ringBuffer`. The header row for each task reflects state:

| State     | Glyph             |
| --------- | ----------------- |
| pending   | `○ `              |
| running   | `⠋ ` (animated)   |
| done      | `✔ `              |
| failed    | `✘ `              |
| skipped   | `— `              |

## Notes

- Cache format changed from v1: now `raw\tmodule\tgroup\tdesc\n` (4 fields, tab-separated). Old fish caches are ignored — first v2 run regenerates them.
- `gradlew.bat` is preferred on Windows; `gradlew` elsewhere.
- `cmd.WaitDelay = 10s` is set so a stuck child process can't pin us forever.
- `programRef.Send` is unbuffered; the streaming channel drops on full to keep the subprocess from blocking on backpressure.

## License

MIT (or whatever you pick — this started as a personal port).
