package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/y2/go-sre-agent/internal/llm"
	memory "github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
	sessionstore "github.com/y2/go-sre-agent/internal/session"
	"github.com/y2/go-sre-agent/internal/tools"
)

type interactiveOptions struct {
	ConfigPath             string
	Environment            string
	SessionID              string
	RunDir                 string
	SessionDir             string
	MemoryDir              string
	MockScenario           string
	OverwriteSessionMemory bool
	Input                  io.Reader
	Output                 io.Writer
	ProgressOutput         io.Writer
	Signals                <-chan os.Signal
	Now                    func() time.Time
}

type interactiveDependencies struct {
	startDiagnosis  func(context.Context, diagnoseOptions, string) (diagnoseResult, error)
	saveDiagnosis   func(diagnoseResult, error) error
	resumeDiagnosis func(context.Context, resumeOptions, string) (diagnoseResult, error)
	modelEvaluation func(context.Context, modelEvaluationOptions) (storedEvaluation, error)
}

type interactivePendingKind string

const (
	pendingResumeSelection     interactivePendingKind = "resume_selection"
	pendingResumeRunning       interactivePendingKind = "resume_running"
	pendingEvalModel           interactivePendingKind = "eval_model"
	pendingMemoryInvalidate    interactivePendingKind = "memory_invalidate"
	pendingMemoryCorrectStatus interactivePendingKind = "memory_correct_status"
	pendingMemoryCorrectNote   interactivePendingKind = "memory_correct_note"
	pendingMemoryDeleteReason  interactivePendingKind = "memory_delete_reason"
	pendingMemoryDeleteConfirm interactivePendingKind = "memory_delete_confirm"
)

type interactivePending struct {
	kind       interactivePendingKind
	runID      string
	scenario   string
	status     string
	reason     string
	candidates map[string]struct{}
}

type interactiveLine struct {
	text string
	err  error
}

type terminalWriter struct {
	writer io.Writer
	mutex  *sync.Mutex
}

type interactiveCLI struct {
	options        interactiveOptions
	deps           interactiveDependencies
	output         *terminalWriter
	progress       *cliProgressWriter
	config         diagnoseOptions
	currentSession string
	newSession     bool
	latestRun      string
	modelLabel     string
	pending        *interactivePending
	runStore       *runstore.Store
	sessionStore   *sessionstore.Store
	memoryStore    *memory.Store
}

// runInteractive 创建逐行终端会话。调用方负责判断 stdin 是否为终端并注册进程信号。
func runInteractive(options interactiveOptions) error {
	cli, err := newInteractiveCLI(options, interactiveDependencies{})
	if err != nil {
		return err
	}
	cli.printBanner()
	return cli.loop()
}

// newInteractiveCLI 使用现有配置、run、session 和 memory 服务创建可注入测试的交互状态。
func newInteractiveCLI(options interactiveOptions, deps interactiveDependencies) (*interactiveCLI, error) {
	if options.Input == nil {
		options.Input = os.Stdin
	}
	if options.Output == nil {
		options.Output = os.Stdout
	}
	if options.ProgressOutput == nil {
		options.ProgressOutput = options.Output
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if strings.TrimSpace(options.MemoryDir) == "" {
		options.MemoryDir = memory.DefaultDir
	}
	if deps.startDiagnosis == nil {
		deps.startDiagnosis = startDiagnosisRun
	}
	if deps.saveDiagnosis == nil {
		deps.saveDiagnosis = saveDiagnosisResult
	}
	if deps.resumeDiagnosis == nil {
		deps.resumeDiagnosis = resumeDiagnosisRun
	}
	if deps.modelEvaluation == nil {
		deps.modelEvaluation = runModelEvaluation
	}

	resolved, err := resolveDiagnosisConfig(diagnoseOptions{
		ConfigPath:             options.ConfigPath,
		RunDir:                 options.RunDir,
		SessionDir:             options.SessionDir,
		MemoryDir:              options.MemoryDir,
		Environment:            options.Environment,
		OverwriteSessionMemory: options.OverwriteSessionMemory,
	})
	if err != nil {
		return nil, err
	}
	cli := &interactiveCLI{
		options: options,
		deps:    deps,
		output:  newTerminalWriter(options.Output),
		config: diagnoseOptions{
			ConfigPath:             options.ConfigPath,
			Service:                resolved.Service,
			RunDir:                 resolved.RunDir,
			SessionDir:             resolved.SessionDir,
			MemoryDir:              options.MemoryDir,
			Environment:            resolved.Environment,
			OverwriteSessionMemory: options.OverwriteSessionMemory,
		},
		modelLabel:   interactiveModelName(options.MockScenario),
		runStore:     runstore.NewStore(resolved.RunDir),
		sessionStore: sessionstore.NewStore(resolved.SessionDir),
		memoryStore:  memory.NewStore(options.MemoryDir),
	}
	cli.progress = newCLIProgressWriter(newTerminalWriterWithMutex(options.ProgressOutput, cli.output.mutex))

	if sessionID := strings.TrimSpace(options.SessionID); sessionID != "" {
		state, err := cli.sessionStore.Load(sessionID)
		if err != nil {
			return nil, fmt.Errorf("load interactive session %q: %w", sessionID, err)
		}
		if state.Environment != cli.config.Environment {
			return nil, fmt.Errorf("session %q belongs to environment %q, not %q", state.SessionID, state.Environment, cli.config.Environment)
		}
		cli.currentSession = state.SessionID
		if len(state.RunIDs) > 0 {
			cli.latestRun = state.RunIDs[len(state.RunIDs)-1]
		}
	} else {
		cli.currentSession = sessionstore.NewID(options.Now().UTC())
		cli.newSession = true
	}
	return cli, nil
}

// newTerminalWriter 创建串行、脱敏且会清除终端控制字符的输出边界。
func newTerminalWriter(writer io.Writer) *terminalWriter {
	return newTerminalWriterWithMutex(writer, &sync.Mutex{})
}

// newTerminalWriterWithMutex 让 stdout 与 stderr 共享渲染锁，避免进度和结果交错。
func newTerminalWriterWithMutex(writer io.Writer, mutex *sync.Mutex) *terminalWriter {
	return &terminalWriter{writer: writer, mutex: mutex}
}

// Write 在所有交互输出进入终端前完成脱敏和控制字符清理，避免外部工具内容影响终端状态。
func (w *terminalWriter) Write(data []byte) (int, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	_, err := io.WriteString(w.writer, terminalText(string(data)))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// terminalText 去除危险控制字符并脱敏可见内容；换行保留给逐行终端展示使用。
func terminalText(value string) string {
	value = tools.RedactSensitive(value)
	return strings.Map(func(character rune) rune {
		if character == '\n' {
			return character
		}
		if character == '\t' {
			return ' '
		}
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, value)
}

// interactiveModelName 返回启动横幅可安全显示的模型名称或 mock 模式，不创建模型连接。
func interactiveModelName(mockScenario string) string {
	if strings.TrimSpace(mockScenario) != "" {
		return "mock/" + mockScenario
	}
	config, err := loadLLMConfig()
	if err != nil || config.Provider == "" || config.Provider == llm.DefaultLLMProvider {
		return "mock"
	}
	if strings.TrimSpace(config.Model) == "" {
		return config.Provider
	}
	return config.Provider + "/" + config.Model
}

// printBanner 输出启动时的最小上下文，不加载历史全文也不执行诊断。
func (c *interactiveCLI) printBanner() {
	fmt.Fprintln(c.output, "SRE Agent")
	fmt.Fprintf(c.output, "环境：%s  模式：%s\n", c.config.Environment, c.modelLabel)
	fmt.Fprintf(c.output, "会话：%s\n", c.currentSession)
	fmt.Fprintln(c.output, "输入问题开始诊断，/help 查看命令。")
}

// loop 接收逐行输入；空闲时 Ctrl-C 仅取消当前输入，Ctrl-D 和 /exit 正常结束会话。
func (c *interactiveCLI) loop() error {
	lines := readInteractiveLines(c.options.Input)
	for {
		fmt.Fprint(c.output, "\nsre > ")
		select {
		case signal := <-c.options.Signals:
			if signal == syscall.SIGTERM {
				fmt.Fprintln(c.output, "收到 SIGTERM，交互会话已退出。")
				return nil
			}
			fmt.Fprintln(c.output, "^C")
			continue
		case line, ok := <-lines:
			if !ok {
				fmt.Fprintln(c.output, "")
				return nil
			}
			if line.err != nil {
				return fmt.Errorf("read interactive input: %w", line.err)
			}
			if c.handleLine(line.text) {
				return nil
			}
		}
	}
}

// readInteractiveLines 在独立 goroutine 中读取终端行，使空闲 Ctrl-C 不会永久阻塞主循环。
func readInteractiveLines(reader io.Reader) <-chan interactiveLine {
	lines := make(chan interactiveLine)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		for scanner.Scan() {
			lines <- interactiveLine{text: scanner.Text()}
		}
		if err := scanner.Err(); err != nil {
			lines <- interactiveLine{err: err}
		}
	}()
	return lines
}

// handleLine 分派普通诊断目标、斜杠命令或等待中的交互确认；返回 true 表示应退出。
func (c *interactiveCLI) handleLine(line string) bool {
	line = strings.TrimSpace(line)
	if c.pending != nil {
		return c.handlePending(line)
	}
	if line == "" {
		return false
	}
	if !strings.HasPrefix(line, "/") {
		return c.runDiagnosis(line)
	}
	arguments, err := splitInteractiveCommand(line)
	if err != nil {
		fmt.Fprintf(c.output, "命令参数错误：%v\n", err)
		return false
	}
	if len(arguments) == 0 {
		return false
	}
	command := strings.TrimPrefix(strings.ToLower(arguments[0]), "/")
	switch command {
	case "help":
		c.printHelp(arguments[1:])
	case "diagnose":
		if len(arguments) < 2 {
			fmt.Fprintln(c.output, "用法：/diagnose <问题>")
			return false
		}
		return c.runDiagnosis(strings.Join(arguments[1:], " "))
	case "resume":
		return c.beginResume(arguments[1:])
	case "status":
		c.showStatus(arguments[1:])
	case "report":
		c.showReport(arguments[1:])
	case "runs":
		c.showRuns()
	case "sessions":
		c.showSessions()
	case "use":
		c.useSession(arguments[1:])
	case "new":
		c.beginNewSession()
	case "plan":
		c.showPlan(arguments[1:])
	case "evidence":
		c.showEvidence(arguments[1:])
	case "config":
		c.showConfig()
	case "llm":
		return c.runLLM(arguments[1:])
	case "eval":
		return c.runEval(arguments[1:])
	case "memory":
		return c.runMemory(arguments[1:])
	case "exit", "quit":
		return true
	default:
		fmt.Fprintf(c.output, "未知交互命令：/%s。输入 /help 查看可用命令。\n", command)
	}
	return false
}

// splitInteractiveCommand 解析斜杠命令的空白、单双引号和反斜杠转义，不调用 shell。
func splitInteractiveCommand(input string) ([]string, error) {
	arguments := make([]string, 0, 4)
	var current strings.Builder
	var quote rune
	escaped := false
	active := false
	for _, character := range input {
		if escaped {
			current.WriteRune(character)
			escaped = false
			active = true
			continue
		}
		if character == '\\' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
				active = true
				continue
			}
			current.WriteRune(character)
			active = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			active = true
			continue
		}
		if unicode.IsSpace(character) {
			if active {
				arguments = append(arguments, current.String())
				current.Reset()
				active = false
			}
			continue
		}
		current.WriteRune(character)
		active = true
	}
	if escaped {
		return nil, fmt.Errorf("命令末尾不能是未转义的反斜杠")
	}
	if quote != 0 {
		return nil, fmt.Errorf("命令中的引号没有闭合")
	}
	if active {
		arguments = append(arguments, current.String())
	}
	return arguments, nil
}

// runTask 为一个交互任务创建独立 context；任务期 Ctrl-C 取消并保存，SIGTERM 保存后退出。
func (c *interactiveCLI) runTask(action func(context.Context)) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	received := make(chan os.Signal, 1)
	monitorFinished := make(chan struct{})
	go func() {
		defer close(monitorFinished)
		select {
		case signal := <-c.options.Signals:
			received <- signal
			cancel()
		case <-done:
		}
	}()
	action(ctx)
	close(done)
	<-monitorFinished
	select {
	case signal := <-received:
		if signal == syscall.SIGTERM {
			fmt.Fprintln(c.output, "任务已收到 SIGTERM，已请求取消并保存当前状态。")
			return true
		}
		fmt.Fprintln(c.output, "任务已取消，已返回交互提示符。")
	default:
	}
	return false
}
