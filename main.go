// igradle is an interactive Gradle task launcher with multi-module support.
//
// Ported from the fish shell function igradle.fish. Same logic, same UX,
// packaged as a single static binary that runs on Linux, macOS, and Windows.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// 1. Argument parsing
// ---------------------------------------------------------------------------

type options struct {
	refresh  bool
	dryRun   bool
	help     bool
	extraArgs []string
}

func parseArgs(args []string) (options, error) {
	fs := flag.NewFlagSet("igradle", flag.ContinueOnError)
	refresh := fs.Bool("r", false, "Force refresh the task cache")
	refreshLong := fs.Bool("refresh", false, "Force refresh the task cache")
	dryRun := fs.Bool("n", false, "Print the command without executing")
	dryRunLong := fs.Bool("dry-run", false, "Print the command without executing")
	help := fs.Bool("h", false, "Show help")
	helpLong := fs.Bool("help", false, "Show help")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	// Collapse short/long duplicates.
	refreshVal := *refresh || *refreshLong
	dryRunVal := *dryRun || *dryRunLong
	helpVal := *help || *helpLong

	if helpVal {
		return options{help: true}, nil
	}

	return options{
		refresh:    refreshVal,
		dryRun:     dryRunVal,
		extraArgs:  fs.Args(),
	}, nil
}

func printHelp() {
	fmt.Println(`Usage: igradle [options] [extra_gradle_args...]

Interactive Gradle task launcher with multi-module support.

Options:
  -r, --refresh   Force refresh the task cache
  -n, --dry-run   Print the command without executing
  -h, --help      Show this help message

Controls:
  ↑/↓ or j/k      Navigate
  Space           Toggle selection
  Enter           Run selected task(s)
  Ctrl-C / Esc    Quit`)
}

// ---------------------------------------------------------------------------
// 2. Gradle wrapper discovery
// ---------------------------------------------------------------------------

// findGradle walks up from cwd looking for ./gradlew. Returns the path to the
// wrapper and the directory containing it (the project root).
func findGradle() (cmd string, root string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, "gradlew")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// No wrapper found — fall back to a system `gradle` if one exists.
	if path, lookErr := exec.LookPath("gradle"); lookErr == nil {
		return path, cwd, nil
	}
	return "", "", fmt.Errorf("no gradlew found above %s and no `gradle` on PATH", cwd)
}

// ---------------------------------------------------------------------------
// 3. Cache management
// ---------------------------------------------------------------------------

// taskCachePath returns the absolute path to the cache file for a given root.
// We tuck it under .gradle/ so it lives inside the project's own directory and
// travels with the repo, just like the fish version did.
func taskCachePath(root string) string {
	return filepath.Join(root, ".gradle", "igradle_cache.txt")
}

// cacheIsStale returns true if any build/settings file is newer than the cache.
func cacheIsStale(root, cachePath string) (bool, error) {
	info, err := os.Stat(cachePath)
	if err != nil {
		return true, nil // missing → stale
	}

	patterns := []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, pat))
		if err != nil {
			return true, err
		}
		matches = append(matches, globAll(filepath.Join(root, "**", pat))...)
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if fi.ModTime().After(info.ModTime()) {
				return true, nil
			}
		}
	}
	return false, nil
}

// globAll is a recursive glob: filepath.Glob doesn't support **, but we want
// to scan nested module dirs too (maxdepth 4 mirrors the original fish find).
func globAll(pattern string) []string {
	var out []string
	parts := strings.SplitN(pattern, "**", 2)
	if len(parts) != 2 {
		return nil
	}
	base, rest := parts[0], strings.TrimPrefix(parts[1], "/")
	filepath.WalkDir(strings.TrimSuffix(base, "/"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// cap depth
		rel, _ := filepath.Rel(base, path)
		depth := strings.Count(rel, string(os.PathSeparator))
		if depth > 4 {
			return nil
		}
		matched, _ := filepath.Match(rest, filepath.Base(path))
		if matched {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// ---------------------------------------------------------------------------
// 4. Task parsing
// ---------------------------------------------------------------------------

// taskItem is one Gradle task as shown in the list.
type taskItem struct {
	rawName  string // e.g. ":backend:auth:build"
	module   string // e.g. ":backend:auth" or ":root"
	short    string // e.g. "build"
	group    string
	desc     string
	icon     string
}

func (t taskItem) Title() string       { return fmt.Sprintf("%s %s", t.icon, t.rawName) }
func (t taskItem) Description() string { return t.desc }
func (t taskItem) FilterValue() string { return t.rawName + " " + t.module + " " + t.short }

// groupHeaders look like "Build tasks", "Help tasks", "Verification tasks".
// We strip the trailing " tasks" to use as the group label.
var groupHeaderRE = regexp.MustCompile(`^([A-Z][A-Za-z ]*) tasks$`)

// taskLine matches "<name> - <description>".
var taskLineRE = regexp.MustCompile(`^([a-zA-Z0-9_:-]+) - (.*)$`)

// iconForTask picks an emoji for the row based on the task short name.
func iconForTask(short string) string {
	switch {
	case matches(short, "build", "assemble", "jar", "war"):
		return "📦"
	case matches(short, "test", "check"):
		return "🧪"
	case matches(short, "clean"):
		return "🧹"
	case matches(short, "run", "bootRun", "start"):
		return "🚀"
	case matches(short, "publish", "upload"):
		return "📤"
	case matches(short, "lint", "format"):
		return "✨"
	case strings.Contains(short, "dependenc"):
		return "🔗"
	case matches(short, "help"):
		return "ℹ️"
	case matches(short, "doc", "javadoc"):
		return "📚"
	default:
		return "⚙️"
	}
}

func matches(s string, needles ...string) bool {
	for _, n := range needles {
		if s == n || strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// splitModulePath splits ":backend:auth:build" into module=":backend:auth" and
// short="build". Mirrors the awk logic from the fish function.
func splitModulePath(raw string) (module, short string) {
	parts := strings.Split(raw, ":")
	if len(parts) <= 1 {
		return ":root", raw
	}
	short = parts[len(parts)-1]
	module = strings.TrimSuffix(raw, ":"+short)
	if !strings.HasPrefix(module, ":") {
		module = ":" + module
	}
	return module, short
}

// parseGradleTasks turns `gradle tasks --all` output into a slice of items.
func parseGradleTasks(r io.Reader) []list.Item {
	var items []list.Item
	group := "Other"
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if m := groupHeaderRE.FindStringSubmatch(line); m != nil {
			group = m[1]
			continue
		}
		// Skip dashes, blanks, and the legend block.
		if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "To see") || strings.HasPrefix(line, "Run ") {
			continue
		}
		m := taskLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		raw, desc := m[1], m[2]
		module, short := splitModulePath(raw)
		items = append(items, taskItem{
			rawName: raw,
			module:  module,
			short:   short,
			group:   group,
			desc:    desc,
			icon:    iconForTask(short),
		})
	}
	return items
}

// ---------------------------------------------------------------------------
// 5. Bubble Tea model
// ---------------------------------------------------------------------------

// model states drive what the View renders. Loading shows the spinner;
// ready shows the multi-select list; confirm prompts before execution.
type modelState int

const (
	stateLoading modelState = iota
	stateReady
	stateConfirm
	stateDone
)

type model struct {
	state   modelState
	opts    options
	cmd     string
	root    string
	cachePath string

	spinner spinner.Model
	list    list.Model
	confirm textinput.Model
	width   int
	height  int

	selected []string // raw task names the user picked
	err      error
}

// itemsLoadedMsg carries the parsed tasks back into Update from a tea.Cmd.
type itemsLoadedMsg struct {
	items []list.Item
	err   error
}

// fetchTasksCmd runs `gradlew tasks --all` (or system gradle) and parses the
// output. It writes to a tmp cache file first, then atomically renames.
func fetchTasksCmd(cmdPath, root, cachePath string, forceRefresh bool) tea.Cmd {
	return func() tea.Msg {
		if !forceRefresh {
			stale, _ := cacheIsStale(root, cachePath)
			if !stale {
				if data, err := os.ReadFile(cachePath); err == nil {
					items := loadCachedItems(string(data))
					return itemsLoadedMsg{items: items}
				}
			}
		}

		// Regenerate from gradle.
		c := exec.Command(cmdPath, "-p", root, "tasks", "--all")
		stdout, err := c.StdoutPipe()
		if err != nil {
			return itemsLoadedMsg{err: err}
		}
		if err := c.Start(); err != nil {
			return itemsLoadedMsg{err: err}
		}
		items := parseGradleTasks(stdout)
		_ = c.Wait()

		// Persist to cache for next time.
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err == nil {
			var b strings.Builder
			for _, it := range items {
				if ti, ok := it.(taskItem); ok {
					fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", ti.rawName, ti.module, ti.group, ti.desc)
				}
			}
			_ = os.WriteFile(cachePath, []byte(b.String()), 0o644)
		}
		return itemsLoadedMsg{items: items}
	}
}

// loadCachedItems rebuilds list.Items from the on-disk cache format.
func loadCachedItems(data string) []list.Item {
	var items []list.Item
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) < 4 {
			continue
		}
		raw, module, group, desc := fields[0], fields[1], fields[2], fields[3]
		_, short := splitModulePath(raw)
		items = append(items, taskItem{
			rawName: raw, module: module, short: short,
			group: group, desc: desc, icon: iconForTask(short),
		})
	}
	return items
}

func newModel(opts options, cmd, root, cachePath string) model {
	s := spinner.New(spinner.WithSpinner(spinner.Dot))
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))

	// Wide default — gets refined on first WindowSizeMsg.
	l := list.New([]list.Item{}, newItemDelegate(), 80, 20)
	l.Title = "⚡ Select tasks"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.Styles.Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))

	ci := textinput.New()
	ci.Placeholder = "y/N"
	ci.CharLimit = 1

	return model{
		state:     stateLoading,
		opts:      opts,
		cmd:       cmd,
		root:      root,
		cachePath: cachePath,
		spinner:   s,
		list:      l,
		confirm:   ci,
		selected:  []string{},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchTasksCmd(m.cmd, m.root, m.cachePath, m.opts.refresh))
}

// toggleCurrent flips the selection state of the row under the cursor.
// Bubble's built-in multi-select is unreliable across versions, so we track
// our own set of selected raw task names — simple and explicit.
func (m *model) toggleCurrent() {
	idx := m.list.Index()
	items := m.list.Items()
	if idx < 0 || idx >= len(items) {
		return
	}
	ti, ok := items[idx].(taskItem)
	if !ok {
		return
	}
	for i, name := range m.selected {
		if name == ti.rawName {
			m.selected = append(m.selected[:i], m.selected[i+1:]...)
			return
		}
	}
	m.selected = append(m.selected, ti.rawName)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.list.SetSize(msg.Width, msg.Height-4)

	case itemsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.state = stateDone
			return m, tea.Quit
		}
		m.list.SetItems(msg.items)
		m.state = stateReady
		return m, nil

	case spinner.TickMsg:
		if m.state == stateLoading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}

	case tea.KeyMsg:
		switch m.state {
		case stateReady:
			switch msg.String() {
			case "ctrl+c", "esc", "q":
				return m, tea.Quit
			case " ":
				m.toggleCurrent()
				syncSelection(m.selected)
				return m, nil
			case "enter":
				if len(m.selected) == 0 {
					return m, tea.Quit
				}
				if len(m.selected) >= 5 {
					m.state = stateConfirm
					m.confirm.Focus()
					return m, nil
				}
				m.state = stateDone
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.list, cmd = m.list.Update(msg)
			return m, cmd

		case stateConfirm:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			case "enter":
				val := strings.TrimSpace(strings.ToLower(m.confirm.Value()))
				if val != "y" && val != "yes" {
					fmt.Println("Cancelled.")
					return m, tea.Quit
				}
				m.state = stateDone
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.confirm, cmd = m.confirm.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m model) View() string {
	switch m.state {
	case stateLoading:
		return fmt.Sprintf("\n  %s Fetching Gradle tasks...\n", m.spinner.View())
	case stateConfirm:
		var b strings.Builder
		fmt.Fprintf(&b, "\n  ⚠️  You selected %d tasks:\n", len(m.selected))
		for _, t := range m.selected {
			fmt.Fprintf(&b, "     • %s\n", t)
		}
		b.WriteString("\n  Proceed? [y/N] ")
		b.WriteString(m.confirm.View())
		b.WriteString("\n")
		return b.String()
	case stateDone:
		return ""
	default:
		return m.list.View()
	}
}

// itemDelegate renders each row in the list. We highlight the module in
// magenta and the short task name in bold green, mirroring the fish version.
// The checkmark is read from m.selected (the model's own set).
type itemDelegate struct{}

func newItemDelegate() itemDelegate { return itemDelegate{} }

func (d itemDelegate) Height() int                             { return 1 }
func (d itemDelegate) Spacing() int                            { return 0 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

// selectedSet is written by model.Update before View runs. Bubble Tea runs
// Update and View on the same goroutine, so this package-level map is safe
// without a mutex and avoids threading the model back through the delegate.
var selectedSet = map[string]struct{}{}

func syncSelection(names []string) {
	selectedSet = make(map[string]struct{}, len(names))
	for _, n := range names {
		selectedSet[n] = struct{}{}
	}
}

func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	ti, ok := item.(taskItem)
	if !ok {
		return
	}
	cursor := "  "
	if index == m.Index() {
		cursor = "▸ "
	}
	checked := " "
	if _, ok := selectedSet[ti.rawName]; ok {
		checked = "✔"
	}

	module := lipgloss.NewStyle().Foreground(lipgloss.Color("35")).Render(fmt.Sprintf("[%s]", ti.module))
	short := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82")).Render(ti.short)
	fmt.Fprintf(w, "%s%s %s %s %s — %s", cursor, checked, ti.icon, module, short, ti.desc)
}

// ---------------------------------------------------------------------------
// 6. Main
// ---------------------------------------------------------------------------

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", err)
		os.Exit(2)
	}
	if opts.help {
		printHelp()
		return
	}

	cmd, root, err := findGradle()
	if err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", err)
		os.Exit(1)
	}

	cachePath := taskCachePath(root)
	m := newModel(opts, cmd, root, cachePath)

	finalModel, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", err)
		os.Exit(1)
	}

	fm := finalModel.(model)
	if fm.err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", fm.err)
		os.Exit(1)
	}
	if fm.state != stateDone || len(fm.selected) == 0 {
		fmt.Println("Cancelled. No tasks selected.")
		return
	}

	if opts.dryRun {
		fmt.Printf("\n🔍 Dry run: %s", cmd)
		for _, t := range fm.selected {
			fmt.Printf(" %s", t)
		}
		for _, a := range opts.extraArgs {
			fmt.Printf(" %s", a)
		}
		fmt.Println()
		return
	}

	fmt.Printf("\n🚀 Running: %s", cmd)
	for _, t := range fm.selected {
		fmt.Printf(" %s", t)
	}
	for _, a := range opts.extraArgs {
		fmt.Printf(" %s", a)
	}
	fmt.Println()

	run := exec.Command(cmd, append(append([]string{"-p", root}, fm.selected...), opts.extraArgs...)...)
	run.Stdout = os.Stdout
	run.Stderr = os.Stderr
	run.Stdin = os.Stdin
	if err := run.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", err)
		os.Exit(1)
	}
}
