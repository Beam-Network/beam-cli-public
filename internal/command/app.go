package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/browser"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	beamupdate "github.com/Beam-Network/beam-cli-public/internal/update"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

type App struct {
	version     version.BuildInfo
	openURL     func(string) error
	isTerminal  func(io.Writer) bool
	updateRun   func(context.Context, beamupdate.Options) (beamupdate.Result, error)
	daemonStart func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error)
	roomRun     func(context.Context, []string, string, string, output.Renderer) error
	authStore   func(string) auth.Store
	readLine    func() (string, error)
}

func New(info version.BuildInfo) *App {
	promptInput := bufio.NewReader(os.Stdin)
	return &App{
		version:     info,
		openURL:     browser.Open,
		isTerminal:  isTerminal,
		updateRun:   beamupdate.Run,
		daemonStart: tunnel.StartDaemon,
		roomRun:     runRoomCommand,
		authStore:   auth.NewStore,
		readLine: func() (string, error) {
			line, err := promptInput.ReadString('\n')
			return strings.TrimRight(line, "\r\n"), err
		},
	}
}

func (a *App) Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	filtered, globals, parseErr := globalArgs(args)
	mode := globals.Mode
	if mode == "" {
		mode = output.Human
	}
	renderer := output.Renderer{
		Mode:           mode,
		Out:            stdout,
		Err:            stderr,
		Interactive:    mode == output.Human && !globals.NoInteractive && a.isTerminal(stderr),
		OutInteractive: mode == output.Human && a.isTerminal(stdout),
		Verbose:        globals.Verbose,
	}
	if parseErr != nil {
		cliErr := asCLIError(parseErr)
		renderer.ErrorWithKind(cliErr.Code, cliErr.Kind, cliErr.Message, cliErr.Hint)
		return cliErr.Code
	}
	if len(filtered) == 1 && filtered[0] == "--version" {
		filtered[0] = "version"
	}
	if isConfigIndependent(filtered) {
		paths, _ := config.ResolvePaths()
		paths.ContextName = globals.Context
		return a.finish(ctx, filtered, config.Config{}, paths, renderer)
	}
	cfg, paths, err := config.LoadContext(globals.Context)
	if globals.Mode == "" && err == nil {
		renderer.Mode = output.Mode(cfg.Output)
		renderer.Interactive = renderer.Mode == output.Human && !globals.NoInteractive && a.isTerminal(stderr)
		renderer.OutInteractive = renderer.Mode == output.Human && a.isTerminal(stdout)
	}
	if err != nil {
		renderer.Error(ExitConfig, "Beam configuration is invalid: "+err.Error()+".", applyReleaseNames("Run `beam config validate` or check config.json and BEAM_* environment variables."))
		return ExitConfig
	}
	return a.finish(ctx, filtered, cfg, paths, renderer)
}

func (a *App) finish(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) int {
	err := a.execute(ctx, args, cfg, paths, renderer)
	// After the command, so a check never delays the work the user asked for.
	defer a.emitUpdateNotice(ctx, cfg, paths, renderer)
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, context.Canceled) {
		renderer.Error(ExitInterrupted, "Command interrupted.", "")
		return ExitInterrupted
	}
	cliErr := asCLIError(err)
	cliErr.Message = applyReleaseNames(cliErr.Message)
	cliErr.Hint = applyReleaseNames(cliErr.Hint)
	if renderer.Verbose && cliErr.Cause != nil {
		if cliErr.Hint != "" {
			cliErr.Hint += " "
		}
		cliErr.Hint += "Cause: " + cliErr.Cause.Error()
	}
	renderer.ErrorWithKind(cliErr.Code, cliErr.Kind, cliErr.Message, cliErr.Hint)
	return cliErr.Code
}

func isConfigIndependent(args []string) bool {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" || args[0] == "version" || args[0] == "--version" || args[0] == "completion" {
		return true
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	if len(args) == 2 && args[0] == "config" && (args[1] == "path" || args[1] == "validate") {
		return true
	}
	return false
}

func isTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
}

func (a *App) execute(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return renderHelp(nil, renderer)
	}
	if args[0] == "help" {
		return renderHelp(helpTopicArgs(args[1:]), renderer)
	}
	for index, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			return renderHelp(args[:index+1], renderer)
		}
	}
	switch args[0] {
	case "--version":
		if len(args) != 1 {
			return usage("--version does not accept arguments")
		}
		fallthrough
	case "version":
		if len(args) != 1 {
			return usage("version does not accept arguments")
		}
		return renderer.Result(a.version, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s %s (%s, %s)\n", version.CLIName(), a.version.Version, a.version.Commit, a.version.Date)
			return err
		})
	case "completion":
		return a.completion(args[1:], renderer)
	case "setup":
		return a.setup(ctx, args[1:], cfg, paths, renderer)
	case "status":
		return a.overallStatus(ctx, args[1:], cfg, paths, renderer)
	case "doctor":
		return a.doctor(ctx, args[1:], cfg, paths, renderer)
	case "logs":
		if err := validateOptions(args[1:], nil, map[string]bool{"--follow": true}); err != nil {
			return err
		}
		client, err := a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		return tunnelLogs(ctx, args[1:], client, renderer)
	case "login", "logout", "whoami":
		return a.authentication(ctx, append([]string{args[0]}, args[1:]...), cfg, paths, renderer)
	case "auth":
		return a.authentication(ctx, args[1:], cfg, paths, renderer)
	case "session":
		return a.sessionCommand(ctx, args[1:], cfg, paths, renderer)
	case "org":
		return a.organizationCommand(ctx, args[1:], cfg, paths, renderer)
	case "config":
		return a.configCommand(ctx, args[1:], cfg, paths, renderer)
	case "context":
		return a.contextCommand(args[1:], cfg, paths, renderer)
	case "share":
		return a.share(ctx, args[1:], cfg, paths, renderer)
	case "receive":
		return a.receive(ctx, args[1:], cfg, paths, renderer)
	case "unshare":
		return a.unshare(ctx, args[1:], cfg, paths, renderer)
	case "transfer":
		return a.transferCommand(ctx, args[1:], cfg, renderer)
	case "operation":
		return a.operationCommand(ctx, args[1:], cfg, renderer)
	case "budget":
		return a.budget(ctx, args[1:], cfg, paths, renderer)
	case "studio":
		return a.studioCommand(ctx, args[1:], cfg, renderer)
	case "action":
		return a.action(ctx, args[1:], cfg, paths, renderer)
	case "agent":
		return a.agent(ctx, args[1:], cfg, paths, renderer)
	case "registry":
		return a.registry(ctx, args[1:], cfg, paths, renderer)
	case "room":
		return a.roomCommand(ctx, args[1:], cfg, paths, renderer)
	case "tunnel":
		return a.tunnel(ctx, args[1:], cfg, paths, renderer)
	case "update":
		return a.update(ctx, args[1:], cfg, renderer)
	default:
		return usage(fmt.Sprintf("unknown command %q", args[0]))
	}
}

type globalOptions struct {
	Mode          output.Mode
	Context       string
	NoInteractive bool
	Verbose       bool
}

func globalArgs(args []string) ([]string, globalOptions, error) {
	var result []string
	var options globalOptions
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--json":
			if options.Mode == output.Quiet {
				return nil, globalOptions{}, usage("--json and --quiet cannot be used together")
			}
			options.Mode = output.JSON
		case "--quiet", "-q":
			if options.Mode == output.JSON {
				return nil, globalOptions{}, usage("--json and --quiet cannot be used together")
			}
			options.Mode = output.Quiet
		case "--no-interactive":
			options.NoInteractive = true
		case "--verbose", "-v":
			options.Verbose = true
		case "--context", "-c":
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "-") {
				return nil, globalOptions{}, usage(arg + " requires a value")
			}
			options.Context = args[index]
		default:
			if strings.HasPrefix(arg, "--context=") {
				options.Context = strings.TrimPrefix(arg, "--context=")
				if options.Context == "" {
					return nil, globalOptions{}, usage("--context requires a value")
				}
			} else {
				result = append(result, arg)
			}
		}
	}
	return result, options, nil
}

func usage(message string) *Error {
	return cliError(ExitUsage, message+".", `Run "beam help" for usage.`, nil)
}

func requiredFlag(args []string, name string) (string, error) {
	value, ok, err := flag(args, name)
	if err != nil {
		return "", err
	}
	if !ok || value == "" {
		return "", usage(name + " is required")
	}
	return value, nil
}

func flag(args []string, name string) (string, bool, error) {
	for index, arg := range args {
		if arg == name {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
				return "", true, usage(name + " requires a value")
			}
			return args[index+1], true, nil
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), true, nil
		}
	}
	return "", false, nil
}

func positional(args []string) ([]string, error) {
	var result []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "--") {
			if strings.Contains(arg, "=") {
				continue
			}
			index++
			if index >= len(args) {
				return nil, usage(arg + " requires a value")
			}
			continue
		}
		result = append(result, arg)
	}
	return result, nil
}

func positionalsForOptions(args []string, valueFlags, boolFlags map[string]bool) ([]string, error) {
	var result []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			result = append(result, arg)
			continue
		}
		name := arg
		if position := strings.IndexByte(arg, '='); position >= 0 {
			name = arg[:position]
			if !valueFlags[name] {
				return nil, usage("unknown option " + name)
			}
			continue
		}
		if boolFlags[name] {
			continue
		}
		if !valueFlags[name] || index+1 >= len(args) {
			return nil, usage(name + " requires a value")
		}
		index++
	}
	return result, nil
}

func has(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func validateOptions(args []string, valueFlags, boolFlags map[string]bool) error {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name := arg
		if position := strings.IndexByte(arg, '='); position >= 0 {
			name = arg[:position]
			if !valueFlags[name] {
				return usage("unknown option " + name)
			}
			if arg[position+1:] == "" {
				return usage(name + " requires a value")
			}
			continue
		}
		if boolFlags[name] {
			continue
		}
		if !valueFlags[name] {
			return usage("unknown option " + name)
		}
		index++
		if index >= len(args) || strings.HasPrefix(args[index], "-") {
			return usage(name + " requires a value")
		}
	}
	return nil
}
