# igradle

Interactive Gradle task launcher with multi-module support.
A single static binary, a port of the fish shell function of the same name.

```
⚡ Select tasks
▸ 📦 [   :backend:auth] build              — Assembles the outputs of this project.
  🧪 [   :backend:auth] test               — Runs the unit tests.
  🚀 [   :backend      ] bootRun           — Runs this project as a Spring Boot application.
  ...
```

## Features

- **Multi-select** — pick one or many tasks, run them in one shot
- **Multi-module aware** — tasks are tagged with their module path
- **Smart cache** — reuses `gradle tasks --all` output until a `build.gradle*`
  or `settings.gradle*` changes
- **Auto-finds `gradlew`** — walks up the directory tree until it finds one,
  falls back to a system `gradle` if not
- **Confirmation** — prompts before running 5+ tasks to avoid accidents
- **Cross-platform** — Linux, macOS, Windows; amd64 and arm64
- **Single binary** — no runtime, no fzf dependency, no awk, no shell

## Install

### From source

```bash
git clone <this-repo> igradle
cd igradle
make install      # puts ./bin/igradle into $GOBIN (or ~/go/bin)
```

### Pre-built binaries

```bash
make build-all
ls bin/           # igradle-linux-amd64, igradle-darwin-arm64, ...
```

Copy the one for your platform onto your `PATH`.

## Usage

```bash
igradle                       # launch the selector
igradle -r                    # force-refresh the task cache
igradle -n                    # dry run — print the command, don't execute
igradle build test            # pass extra args straight to gradle
igradle -h                    # show help
```

### Controls

| Key             | Action                  |
| --------------- | ----------------------- |
| `↑` / `↓`       | Move cursor             |
| `j` / `k`       | Move cursor (vim-style) |
| `/`             | Filter list             |
| `space` / `tab` | Toggle selection        |
| `enter`         | Run selected task(s)    |
| `ctrl-c` / `q`  | Quit                    |

When you've selected 5 or more tasks, igradle asks for confirmation before
running — same as the fish version.

## Architecture

```
main.go          ~ single-file port of the fish function
                 ~ 4 logical sections: arg parsing, gradle discovery,
                   cache, task parsing, Bubble Tea model, run
Makefile         ~ cross-compilation targets
go.mod           ~ module: igradle
```

The Bubble Tea program uses a `model` with three states:

1. **`stateLoading`** — spinner runs while `gradle tasks --all` is fetched in
   a `tea.Cmd`. Once the items arrive, we transition.
2. **`stateReady`** — the multi-select list. Filter, navigate, toggle.
3. **`stateConfirm`** — when ≥5 tasks are selected, prompt before run.
4. **`stateDone`** — exits the alt-screen and runs the chosen command.

The cache file lives at `<root>/.gradle/igradle_cache.txt`. A `find`-style
walker checks if any `build.gradle*` or `settings.gradle*` is newer than
the cache; if so, we re-run `gradle tasks --all`.

## Compared to the fish version

| Fish function            | Go port                                      |
| ------------------------ | -------------------------------------------- |
| `fzf -m`                 | `bubbles/list` multi-select                  |
| `fzf --preview`          | delegate `Description()` (full description)  |
| awk regex split          | `regexp.MustCompile` + `strings.Split`       |
| `find -newer`            | `cacheIsStale` with `filepath.WalkDir`       |
| `set -l cmd gradlew`     | `findGradle()` returns `(cmd, root)`         |
| `read -P confirm`        | `textinput.Model` for `y/N`                  |

## Planned (not in v1)

- Animations on state transitions
- A progress bar during long gradle runs
- Live output streaming inside the TUI (instead of dumping to stdout after exit)
- Per-task fuzzy weight tuning

These are all easy follow-ups; the structure in `main.go` already separates
parsing, UI, and execution so each can be replaced without touching the others.

## License

MIT (or whatever you pick — this started as a personal port).
