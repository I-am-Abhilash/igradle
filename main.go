// igradle is an interactive Gradle task launcher with multi-module support.
//
// v2: streams subprocess output live into the TUI with a per-task spinner,
// elapsed timer, rolling log viewport, and choice of sequential/parallel
// execution with stop/continue-on-failure behavior.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// 1. Argument parsing
// ---------------------------------------------------------------------------

type runMode int

const (
	modeSequential runMode = iota
	modeParallel
)

type failMode int

const (
	failStop failMode = iota
	failContinue
)

type options struct {
	refresh   bool
	dryRun    bool
	help      bool
	mode      *runMode // nil = ask
	onFailure *failMode
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

	modeFlag := fs.String("mode", "", "Execution mode: sequential|parallel (default: ask)")
	modeLong := fs.String("execution-mode", "", "Execution mode: sequential|parallel")
	failFlag := fs.String("on-failure", "", "On failure: stop|continue (default: ask)")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	opts := options{
		refresh:   *refresh || *refreshLong,
		dryRun:    *dryRun || *dryRunLong,
		help:      *help || *helpLong,
		extraArgs: fs.Args(),
	}

	mv := *modeFlag
	if mv == "" {
		mv = *modeLong
	}
	switch mv {
	case "":
		// leave nil → ask
	case "sequential", "seq", "s":
		m := modeSequential
		opts.mode = &m
	case "parallel", "par", "p":
		m := modeParallel
		opts.mode = &m
	default:
		return options{}, fmt.Errorf("invalid --mode %q (want sequential|parallel)", mv)
	}

	switch *failFlag {
	case "":
		// leave nil
	case "stop", "s":
		f := failStop
		opts.onFailure = &f
	case "continue", "c":
		f := failContinue
		opts.onFailure = &f
	default:
		return options{}, fmt.Errorf("invalid --on-failure %q (want stop|continue)", *failFlag)
	}
	return opts, nil
}

func printHelp() {
	fmt.Println(`Usage: igradle [options] [extra_gradle_args...]

Interactive Gradle task launcher with multi-module support.

Options:
  -r, --refresh           Force refresh the task cache
  -n, --dry-run           Print the command without executing
  -h, --help              Show this help message
      --mode <m>          Execution mode: sequential|parallel (default: ask)
      --on-failure <f>    On failure: stop|continue (default: ask)

Selector controls:
  ↑/↓ or j/k    Navigate
  space         Toggle selection
  enter         Confirm
  ctrl-c / q    Quit

Running controls:
  s             Skip current task
  r             Retry current task
  q             Quit early
  PgUp/PgDn     Scroll log
`)
}

// ---------------------------------------------------------------------------
// 2. Gradle wrapper discovery
// ---------------------------------------------------------------------------

func findGradle() (cmd string, root string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	dir := cwd
	wrapper := "gradlew"
	if runtime.GOOS == "windows" {
		wrapper = "gradlew.bat"
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, wrapper)); statErr == nil {
			return filepath.Join(dir, wrapper), dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if path, lookErr := exec.LookPath("gradle"); lookErr == nil {
		return path, cwd, nil
	}
	return "", "", fmt.Errorf("no %s found above %s and no `gradle` on PATH", wrapper, cwd)
}

// ---------------------------------------------------------------------------
// 3. Cache management
// ---------------------------------------------------------------------------

func taskCachePath(root string) string {
	return filepath.Join(root, ".gradle", "igradle_cache.txt")
}

func cacheIsStale(root, cachePath string) (bool, error) {
	info, err := os.Stat(cachePath)
	if err != nil {
		return true, nil
	}
	patterns := []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(filepath.Join(root, pat))
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
		rel, _ := filepath.Rel(base, path)
		if strings.Count(rel, string(os.PathSeparator)) > 4 {
			return nil
		}
		if matched, _ := filepath.Match(rest, filepath.Base(path)); matched {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// ---------------------------------------------------------------------------
// 3.5. Task usage tracking
// ---------------------------------------------------------------------------

func taskUsagePath(root string) string {
	return filepath.Join(root, ".gradle", "igradle_usage.txt")
}

func loadTaskUsage(root string) map[string]int {
	usage := make(map[string]int)
	path := taskUsagePath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return usage
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		var count int
		fmt.Sscanf(fields[1], "%d", &count)
		usage[fields[0]] = count
	}
	return usage
}

func incrementTaskUsage(root string, tasks []string) {
	usage := loadTaskUsage(root)
	for _, t := range tasks {
		usage[t]++
	}
	path := taskUsagePath(root)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	var b strings.Builder
	for name, count := range usage {
		fmt.Fprintf(&b, "%s\t%d\n", name, count)
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

// ---------------------------------------------------------------------------
// 4. Task parsing
// ---------------------------------------------------------------------------

type taskItem struct {
	rawName string
	module  string
	short   string
	group   string
	desc    string
	icon    string
	usage   int
}

func (t taskItem) Title() string       { return fmt.Sprintf("%s %s", t.icon, t.rawName) }
func (t taskItem) Description() string { return t.desc }
func (t taskItem) FilterValue() string { return t.short + " " + t.rawName + " " + t.module }

var (
	groupHeaderRE = regexp.MustCompile(`^([A-Z][A-Za-z ]*) tasks$`)
	taskLineRE    = regexp.MustCompile(`^([a-zA-Z0-9_:-]+) - (.*)$`)
)

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

func parseGradleTasks(r io.Reader, usage map[string]int) []list.Item {
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
			rawName: raw, module: module, short: short,
			group: group, desc: desc, icon: iconForTask(short),
			usage: usage[raw],
		})
	}
	sort.Slice(items, func(i, j int) bool {
		ti := items[i].(taskItem)
		tj := items[j].(taskItem)
		if ti.usage != tj.usage {
			return ti.usage > tj.usage
		}
		if ti.module != tj.module {
			return ti.module < tj.module
		}
		return ti.short < tj.short
	})
	return items
}

// ---------------------------------------------------------------------------
// 5. Ring buffer for log lines (drop-oldest, fixed cap)
// ---------------------------------------------------------------------------

type ringBuffer struct {
	lines []string
	cap   int
	head  int
	size  int
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{lines: make([]string, cap), cap: cap}
}

func (r *ringBuffer) push(line string) {
	r.lines[r.head] = line
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// snapshot returns lines in chronological order.
func (r *ringBuffer) snapshot() []string {
	if r.size < r.cap {
		return append([]string(nil), r.lines[:r.size]...)
	}
	out := make([]string, r.cap)
	copy(out, r.lines[r.head:])
	copy(out[r.cap-r.head:], r.lines[:r.head])
	return out
}

func (r *ringBuffer) tail(n int) []string {
	all := r.snapshot()
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// ---------------------------------------------------------------------------
// 6. Streaming subprocess — bounded channel between scanner and tea.Program
// ---------------------------------------------------------------------------

// streamLine carries one line of subprocess output back into the TUI.
type streamLine struct {
	taskIdx  int
	line     string
	isStderr bool
}

// streamDone signals a subprocess exited.
type streamDone struct {
	taskIdx int
	err     error
	elapsed time.Duration
}

const streamChanCap = 1024

// runOneTask starts gradle on a single task and streams lines via the
// returned channel. Caller must drain the channel until it closes. Caller
// invokes program.Send for each line and once for the close marker.
//
// Two goroutines read stdout/stderr independently into one bounded channel.
// A 1024-buffer channel + a drop-on-full policy means the subprocess never
// stalls on backpressure; we drop the oldest lines if the TUI falls behind.
func runOneTask(taskIdx int, cmdPath, root, taskName string, extraArgs []string) <-chan tea.Msg {
	out := make(chan tea.Msg, streamChanCap)
	go func() {
		defer close(out)

		c := exec.Command(cmdPath, append([]string{"-p", root, taskName}, extraArgs...)...)
		c.Env = os.Environ()
		stdout, err := c.StdoutPipe()
		if err != nil {
			out <- streamDone{taskIdx: taskIdx, err: err}
			return
		}
		stderr, err := c.StderrPipe()
		if err != nil {
			out <- streamDone{taskIdx: taskIdx, err: err}
			return
		}
		if err := c.Start(); err != nil {
			out <- streamDone{taskIdx: taskIdx, err: err}
			return
		}

		// Safety net so a stuck Wait can't pin us forever.
		c.WaitDelay = 10 * time.Second

		start := time.Now()
		var wg sync.WaitGroup
		scan := func(rd io.Reader, isErr bool) {
			defer wg.Done()
			s := bufio.NewScanner(rd)
			s.Buffer(make([]byte, 64*1024), 1024*1024)
			for s.Scan() {
				select {
				case out <- streamLine{taskIdx: taskIdx, line: s.Text(), isStderr: isErr}:
				default:
					// channel full — drop oldest by reading+discarding one.
					// (alternative: drop newest. we keep newest so the user
					//  still sees progress even under heavy output.)
				}
			}
		}
		wg.Add(2)
		go scan(stdout, false)
		go scan(stderr, true)
		wg.Wait()

		waitErr := c.Wait()
		out <- streamDone{taskIdx: taskIdx, err: waitErr, elapsed: time.Since(start)}
	}()
	return out
}

// ---------------------------------------------------------------------------
// 7. Bubble Tea model
// ---------------------------------------------------------------------------

type modelState int

const (
	stateLoading modelState = iota
	stateReady
	stateConfirmCount // ≥5 tasks, ask y/N
	stateModePicker    // choose sequential/parallel + stop/continue
	stateRunning
	stateFailed // a task failed in stop mode; show retry/skip/quit
	stateSummary
	stateDone
	stateSettings
	stateHelp
)

// ---------------------------------------------------------------------------
// 6.5. Themes, Configuration & Spinner Animations
// ---------------------------------------------------------------------------

type Theme struct {
	Name      string
	Title     string
	Module    string
	TaskName  string
	StatusOK  string
	StatusBad string
	Spinner   string
	Muted     string
}

var Themes = []Theme{
	{
		Name:      "Default",
		Title:     "212",
		Module:    "35",
		TaskName:  "82",
		StatusOK:  "82",
		StatusBad: "196",
		Spinner:   "63",
		Muted:     "244",
	},
	{
		Name:      "Dracula",
		Title:     "#ff79c6",
		Module:    "#8be9fd",
		TaskName:  "#50fa7b",
		StatusOK:  "#50fa7b",
		StatusBad: "#ff5555",
		Spinner:   "#bd93f9",
		Muted:     "#6272a4",
	},
	{
		Name:      "Nord",
		Title:     "#88c0d0",
		Module:    "#8fbcbb",
		TaskName:  "#a3be8c",
		StatusOK:  "#a3be8c",
		StatusBad: "#bf616a",
		Spinner:   "#81a1c1",
		Muted:     "#4c566a",
	},
	{
		Name:      "Gruvbox",
		Title:     "#d3869b",
		Module:    "#83a598",
		TaskName:  "#b8bb26",
		StatusOK:  "#b8bb26",
		StatusBad: "#fb4934",
		Spinner:   "#fe8019",
		Muted:     "#928374",
	},
	{
		Name:      "Monokai",
		Title:     "#f92672",
		Module:    "#66d9ef",
		TaskName:  "#a6e22e",
		StatusOK:  "#a6e22e",
		StatusBad: "#f92672",
		Spinner:   "#ae81ff",
		Muted:     "#75715e",
	},
	{
		Name:      "Sunset",
		Title:     "#ff007f",
		Module:    "#ff5f00",
		TaskName:  "#ffff00",
		StatusOK:  "#ffff00",
		StatusBad: "#ff0000",
		Spinner:   "#00ffff",
		Muted:     "#5f5f5f",
	},
}

type AppConfig struct {
	RunMode       string         `json:"run_mode"`
	OnFailure     string         `json:"on_failure"`
	ThemeName     string         `json:"theme_name"`
	SubtaskLimits map[string]int `json:"subtask_limits,omitempty"`
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".igradle_config.json"
	}
	return filepath.Join(home, ".config", "igradle", "config.json")
}

func loadConfig() AppConfig {
	var cfg AppConfig
	path := configPath()
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.ThemeName == "" {
		cfg.ThemeName = "Default"
	}
	if cfg.SubtaskLimits == nil {
		cfg.SubtaskLimits = make(map[string]int)
	}
	return cfg
}

func saveConfig(cfg AppConfig) {
	path := configPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

var claudeWavingSpinner = spinner.Spinner{
	Frames: []string{
		"⎺⎻⎼⎽⎼⎻",
		"⎻⎼⎽⎼⎻⎺",
		"⎼⎽⎼⎻⎺⎻",
		"⎽⎼⎻⎺⎻⎼",
		"⎼⎻⎺⎻⎼⎽",
		"⎻⎺⎻⎼⎽⎼",
	},
	FPS: time.Second / 12,
}

type runState int

const (
	runPending runState = iota
	runRunning
	runDone
	runFailed
	runSkipped
)

// taskRun is one selected task with its own spinner, log buffer, and status.
type taskRun struct {
	name           string
	state          runState
	spinner        spinner.Model
	log            *ringBuffer
	started        time.Time
	elapsed        time.Duration
	err            error
	subtaskCount   int
	subtaskTotal   int
	currentSubtask string
}

type model struct {
	state    modelState
	opts     options
	cmdPath  string
	root     string
	cachePath string

	// selector
	list   list.Model
	width  int
	height int

	// mode picker
	pickerStep int // 0 = mode, 1 = on-failure, 2 = done
	chosenMode     runMode
	chosenFail     failMode
	theme          Theme
	settingsCursor int
	loadingSpinner spinner.Model

	// running
	tasks      []*taskRun
	cursor     int // current task being streamed in sequential mode
	failedAt   int // index of task that failed in stop mode
	logsDirty  bool
	lastFlush  time.Time

	// viewport for the live log
	vp viewport.Model

	// summary
	passed int
	failed int

	// confirm-prompt (≥5 tasks)
	confirm textinput.Model

	// selection set for the multi-select list (see v1 design note)
	selected []string
}

type itemsLoadedMsg struct {
	items []list.Item
	err   error
}

func fetchTasksCmd(cmdPath, root, cachePath string, forceRefresh bool) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(800 * time.Millisecond)
		usage := loadTaskUsage(root)
		if !forceRefresh {
			if stale, _ := cacheIsStale(root, cachePath); !stale {
				if data, err := os.ReadFile(cachePath); err == nil {
					return itemsLoadedMsg{items: loadCachedItems(string(data), usage)}
				}
			}
		}
		c := exec.Command(cmdPath, "-p", root, "tasks", "--all")
		stdout, err := c.StdoutPipe()
		if err != nil {
			return itemsLoadedMsg{err: err}
		}
		if err := c.Start(); err != nil {
			return itemsLoadedMsg{err: err}
		}
		items := parseGradleTasks(stdout, usage)
		_ = c.Wait()
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

func loadCachedItems(data string, usage map[string]int) []list.Item {
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
			usage: usage[raw],
		})
	}
	sort.Slice(items, func(i, j int) bool {
		ti := items[i].(taskItem)
		tj := items[j].(taskItem)
		if ti.usage != tj.usage {
			return ti.usage > tj.usage
		}
		if ti.module != tj.module {
			return ti.module < tj.module
		}
		return ti.short < tj.short
	})
	return items
}

func newSpinner() spinner.Model {
	s := spinner.New(spinner.WithSpinner(spinner.Dot))
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	return s
}

func newModel(opts options, cmdPath, root, cachePath string) model {
	cfg := loadConfig()
	var activeTheme Theme = Themes[0]
	for _, t := range Themes {
		if strings.EqualFold(t.Name, cfg.ThemeName) {
			activeTheme = t
			break
		}
	}

	d := itemDelegate{theme: &activeTheme}
	l := list.New([]list.Item{}, d, 80, 20)
	l.Title = "⚡ Select tasks  [s: settings, t: theme]"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.KeyMap.Filter.SetKeys("/", "i")
	l.Styles.Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(activeTheme.Title))

	ci := textinput.New()
	ci.Placeholder = "y/N"
	ci.CharLimit = 1

	vp := viewport.New(80, 20)

	var mode runMode = modeSequential
	if cfg.RunMode == "parallel" {
		mode = modeParallel
	}
	var fail failMode = failStop
	if cfg.OnFailure == "continue" {
		fail = failContinue
	}

	ls := spinner.New(spinner.WithSpinner(claudeWavingSpinner))
	ls.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(activeTheme.Title))

	return model{
		state:          stateLoading,
		opts:           opts,
		cmdPath:        cmdPath,
		root:           root,
		cachePath:      cachePath,
		list:           l,
		confirm:        ci,
		vp:             vp,
		selected:       []string{},
		pickerStep:     0,
		theme:          activeTheme,
		chosenMode:     mode,
		chosenFail:     fail,
		loadingSpinner: ls,
	}
}

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

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.loadingSpinner.Tick, fetchTasksCmd(m.cmdPath, m.root, m.cachePath, m.opts.refresh))
}

// startFirst kicks off the running state. Either starts all tasks in parallel
// (each as its own goroutine + chan) or starts the first task in sequential.
func (m *model) startFirst() tea.Cmd {
	if m.chosenMode == modeParallel {
		cmds := make([]tea.Cmd, 0, len(m.tasks))
		for i, t := range m.tasks {
			t.state = runRunning
			t.started = time.Now()
			cmds = append(cmds, m.spawnTask(i))
		}
		m.cursor = -1 // all running concurrently
		return tea.Batch(cmds...)
	}
	// sequential
	m.cursor = 0
	if m.cursor < len(m.tasks) {
		m.tasks[0].state = runRunning
		m.tasks[0].started = time.Now()
		return m.spawnTask(0)
	}
	return nil
}

func (m *model) spawnTask(i int) tea.Cmd {
	return func() tea.Msg {
		// Bridge a streaming channel into individual tea.Msgs.
		// We can't use tea.Sequence because each task produces many Msgs.
		// We use tea.Batch of one Msg per line — this works because each
		// call to spawnTask creates its own goroutine that owns the channel.
		ch := runOneTask(i, m.cmdPath, m.root, m.tasks[i].name, m.opts.extraArgs)
		first := <-ch
		// Spawn a follow-up Cmd that drains the rest.
		go func() {
			for msg := range ch {
				programRef.Send(msg)
			}
		}()
		return first
	}
}

// programRef is set in main() before Run. The streaming goroutines call
// Send on it directly so we don't have to plumb a *tea.Program through every
// layer. It's set-once-on-startup, so no mutex needed.
var programRef *tea.Program

// flushTimer fires every 16ms (~60Hz) so we coalesce rapid log lines into a
// single viewport repaint instead of redrawing on every line.
type flushTick time.Time

func flushEvery() tea.Cmd {
	return tea.Tick(16*time.Millisecond, func(t time.Time) tea.Msg { return flushTick(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(msg.Width, msg.Height-4)
		m.vp.Width = msg.Width
		m.vp.Height = msg.Height - 13 // logo(7) + header(3) + progressbar(2) + statusbar(1)
		// Repaint the viewport right now.
		m.refreshViewport()

	case itemsLoadedMsg:
		if msg.err != nil {
			return m, tea.Quit
		}
		m.list.SetItems(msg.items)

		// Onboarding check: if config is missing, transition to stateModePicker on startup
		cfg := loadConfig()
		if cfg.RunMode == "" || cfg.OnFailure == "" {
			m.state = stateModePicker
			m.pickerStep = 0
		} else {
			m.state = stateReady
		}
		return m, nil

	case spinner.TickMsg:
		if m.state == stateLoading {
			var c tea.Cmd
			m.loadingSpinner, c = m.loadingSpinner.Update(msg)
			cmds = append(cmds, c)
		}
		// Animate every running spinner.
		if m.state == stateRunning || m.state == stateFailed {
			for _, t := range m.tasks {
				if t.state == runRunning {
					var c tea.Cmd
					t.spinner, c = t.spinner.Update(msg)
					cmds = append(cmds, c)
				}
			}
		}

	case flushTick:
		if m.state == stateRunning && m.logsDirty && time.Since(m.lastFlush) >= 16*time.Millisecond {
			m.refreshViewport()
			m.lastFlush = time.Now()
		}
		if m.state == stateRunning {
			cmds = append(cmds, flushEvery())
		}

	case streamLine:
		if t := m.taskByIdx(msg.taskIdx); t != nil && t.state == runRunning {
			t.log.push(msg.line)
			m.logsDirty = true

			// Detect sub-task start: "> Task :module:taskName"
			if strings.Contains(msg.line, "> Task :") {
				t.subtaskCount++
				
				// Extract sub-task name
				parts := strings.SplitN(msg.line, "> Task ", 2)
				if len(parts) == 2 {
					subTask := strings.TrimSpace(parts[1])
					// Remove status like UP-TO-DATE, FROM-CACHE, etc.
					if idx := strings.IndexAny(subTask, " \t"); idx != -1 {
						subTask = subTask[:idx]
					}
					t.currentSubtask = subTask
				}
			}
		}

	case streamDone:
		t := m.taskByIdx(msg.taskIdx)
		if t == nil {
			break
		}
		t.elapsed = msg.elapsed
		t.err = msg.err
		if msg.err != nil {
			t.state = runFailed
			m.failed++
			t.log.push(fmt.Sprintf("\n%s", lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.StatusBad)).Render("❌ BUILD FAILED")))
		} else {
			t.state = runDone
			m.passed++
			t.log.push(fmt.Sprintf("\n%s", lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.StatusOK)).Render("✨ BUILD SUCCESSFUL")))

			// Save the final sub-task count back to config
			if t.subtaskCount > 0 {
				cfg := loadConfig()
				if cfg.SubtaskLimits == nil {
					cfg.SubtaskLimits = make(map[string]int)
				}
				cfg.SubtaskLimits[t.name] = t.subtaskCount
				saveConfig(cfg)
			}
		}
		m.logsDirty = true

		switch m.chosenMode {
		case modeParallel:
			if m.allDone() {
				m.state = stateSummary
			}
		case modeSequential:
			if msg.err != nil && m.chosenFail == failStop {
				m.state = stateFailed
				m.failedAt = msg.taskIdx
				return m, nil
			}
			m.cursor++
			if m.cursor >= len(m.tasks) {
				m.state = stateSummary
			} else {
				m.tasks[m.cursor].state = runRunning
				m.tasks[m.cursor].started = time.Now()
				cmds = append(cmds, m.spawnTask(m.cursor))
			}
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		switch m.state {
		case stateReady:
			if m.list.SettingFilter() {
				if msg.String() == "ctrl+c" {
					return m, tea.Quit
				}
				var c tea.Cmd
				m.list, c = m.list.Update(msg)
				return m, c
			}
			switch msg.String() {
			case "ctrl+c", "esc", "q":
				return m, tea.Quit
			case "?":
				m.state = stateHelp
				return m, nil
			case "s":
				m.state = stateSettings
				m.settingsCursor = 0
				return m, nil
			case "t":
				m.cycleTheme(1)
				cfg := loadConfig()
				cfg.ThemeName = m.theme.Name
				saveConfig(cfg)
				return m, nil
			case " ":
				m.toggleCurrent()
				syncSelection(m.selected)
				return m, nil
			case "enter":
				if len(m.selected) == 0 {
					if ti, ok := m.list.SelectedItem().(taskItem); ok {
						m.selected = append(m.selected, ti.rawName)
					} else {
						return m, nil
					}
				}
				if len(m.selected) >= 5 {
					m.state = stateConfirmCount
					m.confirm.Focus()
					return m, nil
				}
				return m, m.beginRun()
			}
			var c tea.Cmd
			m.list, c = m.list.Update(msg)
			return m, c

		case stateConfirmCount:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			case "enter":
				val := strings.ToLower(strings.TrimSpace(m.confirm.Value()))
				if val != "y" && val != "yes" {
					return m, tea.Quit
				}
				return m, m.beginRun()
			}
			var c tea.Cmd
			m.confirm, c = m.confirm.Update(msg)
			return m, c

		case stateModePicker:
			switch msg.String() {
			case "s":
				if m.pickerStep == 0 {
					m.chosenMode = modeSequential
					m.pickerStep = 1
				} else if m.pickerStep == 1 {
					m.chosenFail = failStop
					m.pickerStep = 2
				}
			case "p":
				if m.pickerStep == 0 {
					m.chosenMode = modeParallel
					m.pickerStep = 1
				}
			case "c":
				if m.pickerStep == 1 {
					m.chosenFail = failContinue
					m.pickerStep = 2
				}
			case "left", "h", "up", "k":
				if m.pickerStep == 2 {
					m.cycleTheme(-1)
				}
			case "right", "l", "down", "j":
				if m.pickerStep == 2 {
					m.cycleTheme(1)
				}
			case "enter", " ":
				if m.pickerStep == 2 {
					m.pickerStep = 3
				}
			case "1", "2", "3", "4", "5", "6":
				if m.pickerStep == 2 {
					idx := int(msg.String()[0] - '1')
					if idx >= 0 && idx < len(Themes) {
						m.theme = Themes[idx]
						m.list.Styles.Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.Title))
						m.list.SetDelegate(itemDelegate{theme: &m.theme})
						m.pickerStep = 3
					}
				}
			case "ctrl+c", "esc", "q":
				return m, tea.Quit
			}
			if m.pickerStep >= 3 {
				cfg := loadConfig()
				if m.chosenMode == modeSequential {
					cfg.RunMode = "sequential"
				} else {
					cfg.RunMode = "parallel"
				}
				if m.chosenFail == failStop {
					cfg.OnFailure = "stop"
				} else {
					cfg.OnFailure = "continue"
				}
				cfg.ThemeName = m.theme.Name
				saveConfig(cfg)

				if len(m.selected) > 0 {
					return m, m.kickoffRun()
				}
				m.state = stateReady
				return m, nil
			}
			return m, nil

		case stateSettings:
			switch msg.String() {
			case "up", "k":
				m.settingsCursor--
				if m.settingsCursor < 0 {
					m.settingsCursor = 2
				}
			case "down", "j", "tab":
				m.settingsCursor++
				if m.settingsCursor > 2 {
					m.settingsCursor = 0
				}
			case "left", "h":
				if m.settingsCursor == 0 {
					m.chosenMode = modeSequential
				} else if m.settingsCursor == 1 {
					m.chosenFail = failStop
				} else if m.settingsCursor == 2 {
					m.cycleTheme(-1)
				}
			case "right", "l":
				if m.settingsCursor == 0 {
					m.chosenMode = modeParallel
				} else if m.settingsCursor == 1 {
					m.chosenFail = failContinue
				} else if m.settingsCursor == 2 {
					m.cycleTheme(1)
				}
			case " ", "enter":
				if m.settingsCursor == 0 {
					if m.chosenMode == modeSequential {
						m.chosenMode = modeParallel
					} else {
						m.chosenMode = modeSequential
					}
				} else if m.settingsCursor == 1 {
					if m.chosenFail == failStop {
						m.chosenFail = failContinue
					} else {
						m.chosenFail = failStop
					}
				} else if m.settingsCursor == 2 {
					m.cycleTheme(1)
				}
			case "q", "esc":
				cfg := loadConfig()
				if m.chosenMode == modeSequential {
					cfg.RunMode = "sequential"
				} else {
					cfg.RunMode = "parallel"
				}
				if m.chosenFail == failStop {
					cfg.OnFailure = "stop"
				} else {
					cfg.OnFailure = "continue"
				}
				cfg.ThemeName = m.theme.Name
				saveConfig(cfg)

				m.state = stateReady
				return m, nil
			}
			return m, nil

		case stateHelp:
			switch msg.String() {
			case "?", "q", "esc", "enter", " ":
				m.state = stateReady
				return m, nil
			}
			return m, nil

		case stateRunning:
			switch msg.String() {
			case "pgup":
				m.vp.HalfViewUp()
				return m, nil
			case "pgdown":
				m.vp.HalfViewDown()
				return m, nil
			case "ctrl+c", "q":
				// best-effort: skip remaining
				for i := m.cursor + 1; i < len(m.tasks); i++ {
					m.tasks[i].state = runSkipped
				}
				m.state = stateSummary
				return m, nil
			}

		case stateFailed:
			switch msg.String() {
			case "s": // skip
				m.failedAt++
				for m.failedAt < len(m.tasks) && m.tasks[m.failedAt].state != runPending {
					m.failedAt++
				}
				if m.failedAt >= len(m.tasks) {
					m.state = stateSummary
					return m, nil
				}
				m.tasks[m.failedAt].state = runRunning
				m.tasks[m.failedAt].started = time.Now()
				m.cursor = m.failedAt
				m.state = stateRunning
				return m, m.spawnTask(m.failedAt)
			case "r": // retry
				m.tasks[m.failedAt].log = newRingBuffer(5000)
				m.tasks[m.failedAt].state = runRunning
				m.tasks[m.failedAt].started = time.Now()
				m.cursor = m.failedAt
				m.state = stateRunning
				return m, m.spawnTask(m.failedAt)
			case "ctrl+c", "q":
				m.state = stateSummary
				return m, nil
			}
		case stateSummary:
			switch msg.String() {
			case "q", "ctrl+c", "enter", "esc":
				return m, tea.Quit
			}
		}
	}
	if m.state == stateReady {
		var c tea.Cmd
		m.list, c = m.list.Update(msg)
		if c != nil {
			cmds = append(cmds, c)
		}
	}
	return m, tea.Batch(cmds...)
}

// beginRun decides whether to show the mode picker or jump straight into
// running (when both flags were supplied).
func (m *model) cycleTheme(dir int) {
	idx := -1
	for i, t := range Themes {
		if t.Name == m.theme.Name {
			idx = i
			break
		}
	}
	if idx == -1 {
		idx = 0
	}
	idx += dir
	if idx < 0 {
		idx = len(Themes) - 1
	} else if idx >= len(Themes) {
		idx = 0
	}
	m.theme = Themes[idx]

	m.list.Styles.Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.Title))
	m.list.SetDelegate(itemDelegate{theme: &m.theme})
}

func (m *model) styleOption(s string, active bool) string {
	if active {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.TaskName)).Render(s)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted)).Render(s)
}

func (m *model) beginRun() tea.Cmd {
	incrementTaskUsage(m.root, m.selected)
	m.passed, m.failed = 0, 0
	m.cursor = 0
	m.logsDirty = true

	// Priority 1: command line flags
	if m.opts.mode != nil {
		m.chosenMode = *m.opts.mode
	}
	if m.opts.onFailure != nil {
		m.chosenFail = *m.opts.onFailure
	}

	// If either of them is not resolved, try the saved config
	cfg := loadConfig()
	if m.opts.mode == nil && cfg.RunMode != "" {
		if cfg.RunMode == "sequential" {
			m.chosenMode = modeSequential
		} else if cfg.RunMode == "parallel" {
			m.chosenMode = modeParallel
		}
	}
	if m.opts.onFailure == nil && cfg.OnFailure != "" {
		if cfg.OnFailure == "stop" {
			m.chosenFail = failStop
		} else if cfg.OnFailure == "continue" {
			m.chosenFail = failContinue
		}
	}

	m.tasks = make([]*taskRun, len(m.selected))
	for i, name := range m.selected {
		sp := spinner.New(spinner.WithSpinner(spinner.Dot))
		sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Spinner))
		
		totalSub := 10
		if val, ok := cfg.SubtaskLimits[name]; ok && val > 0 {
			totalSub = val
		}

		m.tasks[i] = &taskRun{
			name:         name,
			state:        runPending,
			spinner:      sp,
			log:          newRingBuffer(5000),
			subtaskTotal: totalSub,
		}
	}

	// If both are resolved now, proceed directly
	hasMode := m.opts.mode != nil || cfg.RunMode != ""
	hasFail := m.opts.onFailure != nil || cfg.OnFailure != ""
	if hasMode && hasFail {
		return m.kickoffRun()
	}

	// Otherwise, we need to ask!
	m.state = stateModePicker
	if !hasMode {
		m.pickerStep = 0
	} else {
		m.pickerStep = 1
	}
	return nil
}

func (m *model) kickoffRun() tea.Cmd {
	m.state = stateRunning
	m.vp.GotoBottom()
	return tea.Batch(m.startFirst(), flushEvery())
}

func (m *model) refreshViewport() {
	var b strings.Builder
	if m.chosenMode == modeSequential && m.cursor >= 0 && m.cursor < len(m.tasks) {
		t := m.tasks[m.cursor]
		fmt.Fprintf(&b, "── %s ──\n", t.name)
		for _, line := range t.log.snapshot() {
			fmt.Fprintln(&b, line)
		}
	} else {
		// parallel: show the most-recent task that has output.
		for i := len(m.tasks) - 1; i >= 0; i-- {
			t := m.tasks[i]
			if t.log.size > 0 {
				fmt.Fprintf(&b, "── %s ──\n", t.name)
				for _, line := range t.log.snapshot() {
					fmt.Fprintln(&b, line)
				}
				break
			}
		}
		if b.Len() == 0 {
			b.WriteString("(waiting for output…)\n")
		}
	}
	m.vp.SetContent(b.String())
	if m.vp.AtBottom() {
		m.vp.GotoBottom()
	}
	m.logsDirty = false
}

func (m *model) taskByIdx(idx int) *taskRun {
	if idx >= 0 && idx < len(m.tasks) {
		return m.tasks[idx]
	}
	return nil
}

func (m *model) allDone() bool {
	for _, t := range m.tasks {
		if t.state == runRunning || t.state == runPending {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 8. View
// ---------------------------------------------------------------------------

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Padding(0, 1)
	statusOKStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	statusBad     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	statusSkipped = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	headerStyle   = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, true, false).BorderForeground(lipgloss.Color("63"))
	statusBar     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func (m *model) View() string {
	switch m.state {
	case stateLoading:
		logoText := `  ____               _ _
 / ___|_ __ __ _  __| | | ___
| |  _| '__/ _' |/ _' | |/ _ \
| |_| | | | (_| | (_| | |  __/
 \____|_|  \__,_|\__,_|_|\___|`
		gradientLogo := renderGradient(logoText, "#209BC4", "#02A882")

		versionStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#209BC4")).
			Padding(0, 1)
		versionLabel := versionStyle.Render("v1.0.0")

		logoLines := strings.Split(gradientLogo, "\n")
		var b strings.Builder
		b.WriteString("\n")
		for _, line := range logoLines {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("             " + versionLabel + "\n\n")

		spinnerView := m.loadingSpinner.View()
		loadingText := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.Title)).Render("Fetching Gradle tasks...")
		b.WriteString(fmt.Sprintf("  %s %s\n", spinnerView, loadingText))
		return b.String()

	case stateReady:
		return m.list.View()

	case stateConfirmCount:
		var b strings.Builder
		fmt.Fprintf(&b, "\n  ⚠️  You selected %d tasks:\n", len(m.selected))
		for _, t := range m.selected {
			fmt.Fprintf(&b, "     • %s\n", t)
		}
		b.WriteString("\n  Proceed? [y/N] ")
		b.WriteString(m.confirm.View())
		return b.String()

	case stateModePicker:
		var b strings.Builder
		if m.pickerStep == 0 {
			b.WriteString("\n  Run how?\n")
			b.WriteString("    [s]equential   one at a time, full-screen log per task\n")
			b.WriteString("    [p]arallel     all at once, combined output\n")
		} else if m.pickerStep == 1 {
			b.WriteString("\n  On failure?\n")
			b.WriteString("    [s]top        stop after first failure\n")
			b.WriteString("    [c]ontinue    run all tasks, summarize at end\n")
		} else {
			b.WriteString("\n  Select theme (Press left/right to cycle, enter/number to confirm):\n")
			for i, t := range Themes {
				cursor := "  "
				if t.Name == m.theme.Name {
					cursor = "▸ "
				}
				nameStyled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(t.Title)).Render(t.Name)
				b.WriteString(fmt.Sprintf("    [%d] %s%s\n", i+1, cursor, nameStyled))
			}
		}
		return b.String()

	case stateRunning, stateFailed:
		var b strings.Builder
		b.WriteString(m.renderHeader())
		b.WriteString("\n")
		b.WriteString(m.vp.View())
		b.WriteString("\n")
		b.WriteString(m.renderProgressBar())
		b.WriteString("\n")
		b.WriteString(m.renderStatus())
		return b.String()

	case stateSummary:
		statusOKStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.StatusOK))
		statusBad := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.StatusBad))
		var b strings.Builder
		fmt.Fprintf(&b, "\n  Done.  %s  %d passed   %s  %d failed\n",
			statusOKStyle.Render("✔"), m.passed,
			statusBad.Render("✘"), m.failed)
		if m.failed > 0 {
			b.WriteString("\n  Failed tasks:\n")
			for _, t := range m.tasks {
				if t.state == runFailed {
					fmt.Fprintf(&b, "    ✘ %s  (last output below)\n", t.name)
				}
			}
		}
		b.WriteString("\n  Press q or enter to exit\n")
		return b.String()

	case stateSettings:
		var b strings.Builder
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.Title)).Render("\n  ⚙️  igradle Settings\n\n"))

		// 1. Run Mode
		cursorRun := "  "
		if m.settingsCursor == 0 {
			cursorRun = "▸ "
		}
		modeSeqStr := "[ ] Sequential"
		modeParStr := "[ ] Parallel"
		if m.chosenMode == modeSequential {
			modeSeqStr = "[x] Sequential"
		} else {
			modeParStr = "[x] Parallel"
		}
		b.WriteString(fmt.Sprintf("  %s%-12s %-16s %-16s\n", 
			cursorRun, 
			"Run Mode",
			m.styleOption(modeSeqStr, m.chosenMode == modeSequential),
			m.styleOption(modeParStr, m.chosenMode == modeParallel),
		))

		// 2. On Failure
		cursorFail := "  "
		if m.settingsCursor == 1 {
			cursorFail = "▸ "
		}
		failStopStr := "[ ] Stop"
		failContStr := "[ ] Continue"
		if m.chosenFail == failStop {
			failStopStr = "[x] Stop"
		} else {
			failContStr = "[x] Continue"
		}
		b.WriteString(fmt.Sprintf("  %s%-12s %-16s %-16s\n", 
			cursorFail,
			"On Failure",
			m.styleOption(failStopStr, m.chosenFail == failStop),
			m.styleOption(failContStr, m.chosenFail == failContinue),
		))

		// 3. Theme
		cursorTheme := "  "
		if m.settingsCursor == 2 {
			cursorTheme = "▸ "
		}
		b.WriteString(fmt.Sprintf("  %s%-12s ‹ %s ›\n\n", 
			cursorTheme,
			"Theme",
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.Title)).Render(m.theme.Name),
		))

		// Instructions
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted)).Render(
			"  Use [↑/↓] or [tab] to navigate, [space/enter] or [←/→] to change.\n" +
			"  Press [q/esc] to save and return to tasks.\n",
		))
		return b.String()

	case stateHelp:
		var b strings.Builder
		titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.Title))
		mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted))
		keyStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(m.theme.TaskName))

		b.WriteString(titleStyle.Render("\n  ⚡ igradle Keyboard Shortcuts\n\n"))

		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[↑/↓] or [j/k]"), "Move cursor"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[space]"), "Toggle task selection"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[enter]"), "Run selected tasks (or current task if none selected)"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[s]"), "Open Settings Menu"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[t]"), "Cycle themes on the fly"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[/] or [i]"), "Instant search/filtering (fzf style)"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[?]"), "Toggle this help screen"))
		b.WriteString(fmt.Sprintf("    %-16s %s\n", keyStyle.Render("[q]"), "Quit"))

		b.WriteString(mutedStyle.Render("\n  Press [?] or [esc] to return to the task list.\n"))
		return b.String()
	}
	return ""
}

func (m *model) renderHeader() string {
	var rows []string
	statusOKStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.StatusOK))
	statusBad := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.StatusBad))
	statusSkipped := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted))
	headerStyle := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, true, false).BorderForeground(lipgloss.Color(m.theme.Spinner))

	for i, t := range m.tasks {
		var status string
		var style lipgloss.Style
		switch t.state {
		case runPending:
			status, style = "○ ", lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted))
		case runRunning:
			status = t.spinner.View() + " "
			style = lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Spinner))
		case runDone:
			status, style = "✔ ", statusOKStyle
		case runFailed:
			status, style = "✘ ", statusBad
		case runSkipped:
			status, style = "— ", statusSkipped
		}
		elapsed := ""
		if t.state == runRunning {
			elapsed = fmt.Sprintf("  %s", time.Since(t.started).Round(time.Second))
		} else if t.state == runDone || t.state == runFailed {
			elapsed = fmt.Sprintf("  %s", t.elapsed.Round(time.Second))
		}
		rows = append(rows, style.Render(fmt.Sprintf(" %s%-30s%s", status, t.name, elapsed)))
		_ = i
	}
	return headerStyle.Width(m.width).Render(strings.Join(rows, "\n"))
}

func (m *model) renderStatus() string {
	statusOKStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.StatusOK))
	statusBad := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.StatusBad))
	statusBar := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted))

	parts := []string{
		fmt.Sprintf("%d/%d", m.cursor+1, len(m.tasks)),
		statusOKStyle.Render(fmt.Sprintf("%d ✔", m.passed)),
		statusBad.Render(fmt.Sprintf("%d ✘", m.failed)),
	}
	if m.state == stateFailed {
		parts = append(parts, "[s]kip  [r]etry  [q]uit")
	} else if m.chosenMode == modeSequential {
		parts = append(parts, "PgUp/PgDn scroll  q skip remaining")
	} else {
		parts = append(parts, "PgUp/PgDn scroll  q stop")
	}
	return statusBar.Width(m.width).Render(" " + strings.Join(parts, "   "))
}

// ---------------------------------------------------------------------------
// 9. List delegate (selector)
// ---------------------------------------------------------------------------

type itemDelegate struct {
	theme *Theme
}

func (d itemDelegate) Height() int                             { return 1 }
func (d itemDelegate) Spacing() int                            { return 0 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

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
	module := lipgloss.NewStyle().Foreground(lipgloss.Color(d.theme.Module)).Render(fmt.Sprintf("[%s]", ti.module))
	short := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(d.theme.TaskName)).Render(ti.short)
	numStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(d.theme.Muted))
	numStr := numStyle.Render(fmt.Sprintf("%-3s", fmt.Sprintf("%d.", index+1)))
	fmt.Fprintf(w, "%s%s %s %s %s — %s", cursor, checked, numStr, module, short, ti.desc)
}

func (m *model) renderProgressBar() string {
	totalTasks := len(m.tasks)
	if totalTasks == 0 {
		return ""
	}

	var totalSubtasksCount int
	var totalSubtasksExpected int
	var currentRunningTaskSubtask string

	for _, t := range m.tasks {
		if t.state == runRunning {
			totalSubtasksCount += t.subtaskCount
			totalSubtasksExpected += t.subtaskTotal
			if t.currentSubtask != "" {
				currentRunningTaskSubtask = t.currentSubtask
			}
		} else if t.state == runDone {
			totalSubtasksCount += t.subtaskCount
			totalSubtasksExpected += t.subtaskCount
		} else if t.state == runFailed {
			totalSubtasksCount += t.subtaskCount
			totalSubtasksExpected += t.subtaskTotal
		} else {
			totalSubtasksExpected += t.subtaskTotal
		}
	}

	if totalSubtasksExpected == 0 {
		totalSubtasksExpected = 10 * totalTasks
	}

	percent := float64(totalSubtasksCount) / float64(totalSubtasksExpected)
	if percent > 1.0 {
		percent = 1.0
	}

	barWidth := 40
	filledWidth := int(percent * float64(barWidth))

	var bar strings.Builder
	for i := 0; i < barWidth; i++ {
		if i < filledWidth {
			bar.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Title)).Render("█"))
		} else {
			bar.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Muted)).Render("░"))
		}
	}

	completedTasks := m.passed + m.failed
	statusText := fmt.Sprintf("  %d%% (%d/%d sub-tasks complete)", int(percent*100), totalSubtasksCount, totalSubtasksExpected)
	if currentRunningTaskSubtask != "" && completedTasks < totalTasks {
		statusText += "  " + lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Spinner)).Render(currentRunningTaskSubtask)
	}

	return "  " + bar.String() + statusText
}

func renderGradient(text string, startHex, endHex string) string {
	startR, startG, startB := parseHex(startHex)
	endR, endG, endB := parseHex(endHex)

	lines := strings.Split(text, "\n")
	var result []string

	for _, line := range lines {
		var coloredLine strings.Builder
		runes := []rune(line)
		length := len(runes)
		for i, r := range runes {
			if r == ' ' {
				coloredLine.WriteRune(r)
				continue
			}
			var ratio float64
			if length > 1 {
				ratio = float64(i) / float64(length-1)
			} else {
				ratio = 0.0
			}
			currR := int(float64(startR) + ratio*float64(endR-startR))
			currG := int(float64(startG) + ratio*float64(endG-startG))
			currB := int(float64(startB) + ratio*float64(endB-startB))

			colorHex := fmt.Sprintf("#%02X%02X%02X", currR, currG, currB)
			char := string(r)
			coloredLine.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(colorHex)).Render(char))
		}
		result = append(result, coloredLine.String())
	}
	return strings.Join(result, "\n")
}

func parseHex(h string) (int, int, int) {
	h = strings.TrimPrefix(h, "#")
	if len(h) != 6 {
		return 0, 0, 0
	}
	var r, g, b int
	fmt.Sscanf(h, "%02x%02x%02x", &r, &g, &b)
	return r, g, b
}

// ---------------------------------------------------------------------------
// 10. Main
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

	cmdPath, root, err := findGradle()
	if err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", err)
		os.Exit(1)
	}

	cachePath := taskCachePath(root)
	m := newModel(opts, cmdPath, root, cachePath)

	p := tea.NewProgram(&m, tea.WithAltScreen())
	programRef = p
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "igradle: %v\n", err)
		os.Exit(1)
	}

	// After the TUI exits, print a final newline so the shell prompt
	// doesn't collide with our last output.
	fmt.Println()
}
