package command

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const banner = ` ____  _____    _    __  __
| __ )| ____|  / \  |  \/  |
|  _ \|  _|   / _ \ | |\/| |
| |_) | |___ / ___ \| |  | |
|____/|_____/_/   \_\_|  |_|
`

func renderHelp(path []string, renderer output.Renderer) error {
	topic, ok := helpTopicPath(path)
	if !ok {
		return usage(fmt.Sprintf("unknown help topic %q", strings.Join(path, " ")))
	}
	// --json carries the plain text, never the styled form: the payload is data
	// for another program, and box drawing would be noise inside it.
	return renderer.Result(map[string]any{"command": strings.Join(path, " "), "help": plainTopic(topic)}, func(w io.Writer) error {
		return renderTopic(w, topic, renderer)
	})
}

// helpTopicArgs resolves the topic named by "beam help ...". A help flag asks
// for help rather than naming a topic, so it is dropped: "beam help tunnel
// --help" still means tunnel, and "beam help --help" asks about help itself,
// matching "beam help help". A bare "beam help" keeps meaning the root topic.
func helpTopicArgs(args []string) []string {
	path := make([]string, 0, len(args))
	flagged := false
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			flagged = true
			continue
		}
		path = append(path, arg)
	}
	if flagged && len(path) == 0 {
		return []string{"help"}
	}
	return path
}

func helpTopicPath(path []string) (helpTopic, bool) {
	if len(path) == 0 {
		return rootHelpTopic, true
	}
	if len(path) == 1 {
		switch path[0] {
		case "auth", "login", "logout", "whoami":
			return helpTopics["auth"], true
		case "action", "agent", "budget", "registry", "room", "tunnel", "update":
			return helpTopics[path[0]], true
		case "completion":
			return helpTopic{Usage: []string{"beam completion <install|generate|uninstall> [bash|zsh|fish|powershell]"}}, true
		case "version":
			return helpTopic{Usage: []string{"beam version"}, Summary: "Show the Beam CLI build version."}, true
		}
	}
	key := strings.Join(path, " ")
	if topic, ok := commandTopics[key]; ok {
		return topic, true
	}
	// Until every leaf has distinct prose, the parent help is still more useful
	// than rejecting --help as an unknown option.
	switch path[0] {
	case "auth", "action", "agent", "registry", "room", "tunnel", "update":
		return helpTopics[path[0]], true
	}
	return helpTopic{}, false
}

// helpTopicFor resolves a single named topic for the commands that print their
// own help inline.
func helpTopicFor(name string) helpTopic {
	if topic, ok := helpTopics[name]; ok {
		return topic
	}
	return commandTopics[name]
}

// renderNamedHelp is the shared path for "beam <command> help", so a subcommand
// help page is styled exactly like "beam help <command>".
func renderNamedHelp(name string, renderer output.Renderer) error {
	topic := helpTopicFor(name)
	return renderer.Result(map[string]any{"help": plainTopic(topic)}, func(w io.Writer) error {
		return renderTopic(w, topic, renderer)
	})
}

func usageOnly(usage ...string) helpTopic {
	return helpTopic{Usage: usage}
}

var commandTopics = map[string]helpTopic{
	"setup": {
		Usage:   []string{"beam setup [--mode account|room|skip] [options]"},
		Summary: "Interactively connect a Beam account, join a Room, or defer configuration.",
		Sections: []helpSection{
			{Title: "Account options", Entries: lines("--coordinator URL", "--organization ID|SLUG", "--label NAME", "--enrollment-token TOKEN")},
			{Title: "Room options", Entries: lines("--room ROOM_ID", "--invitation-file FILE | --invitation-token TOKEN", "--coordinator URL")},
			{Title: "General options", Entries: lines("--completion bash|zsh|fish|powershell|none")},
		},
	},
	"status":    {Usage: []string{"beam status"}, Summary: "Show account, context, agent, daemon, tunnels, and operations."},
	"doctor":    {Usage: []string{"beam doctor [--fix]"}, Summary: "Diagnose Beam and optionally apply safe local repairs."},
	"session":   usageOnly("beam session <status|refresh>"),
	"org":       usageOnly("beam org <list|current|use|show> [organization]"),
	"context":   usageOnly("beam context <list|current|create|use|show|delete> [name]"),
	"config":    usageOnly("beam config <path|list|get|set|unset|edit|validate> [arguments]"),
	"share":     usageOnly("beam share <file|port|url> [--name NAME] [--private] [--s3] [--open] [--copy]"),
	"receive":   usageOnly("beam receive DIRECTORY [--name NAME] [--private] [--s3]"),
	"unshare":   usageOnly("beam unshare <name|id|--last>"),
	"transfer":  usageOnly("beam transfer <upload|download|duplex|list|show|watch|retry|cancel|logs> [arguments]"),
	"operation": usageOnly("beam operation <list|show|watch|retry|cancel|logs|prune> [arguments]"),
	"studio":    studioHelpTopic(),
	"logs":      usageOnly("beam logs [--follow]"),
	// "beam help help" previously answered "unknown help topic", even though
	// help is listed as a command on the root page.
	"help": {Usage: []string{"beam help [command]"}, Summary: "Show help for Beam or for one command."},

	"agent connect":   usageOnly("beam agent connect [--coordinator URL] [--organization ID|SLUG] [--label NAME] [--enrollment-token TOKEN]"),
	"agent dashboard": {Usage: []string{"beam agent dashboard [--no-open]"}, Summary: "Start a temporary local dashboard session."},
	"room join":       usageOnly("beam room join ROOM_ID (--invitation-token TOKEN | --invitation-file FILE) [--coordinator URL]"),

	"registry inspect":  usageOnly("beam registry inspect [directory]"),
	"registry pack":     usageOnly("beam registry pack [directory] --out FILE"),
	"registry publish":  usageOnly("beam registry publish [directory] --tag TAG"),
	"registry versions": usageOnly("beam registry versions PACKAGE"),
	"registry resolve":  usageOnly("beam registry resolve PACKAGE [--range RANGE]"),

	"action run": usageOnly("beam action run [directory] [--config FILE] [--inputs FILE] [--secrets FILE] [--runtime mock|live]"),

	"tunnel expose":  usageOnly("beam tunnel expose <file|http|stream|webrtc|tcp> TARGET [options]"),
	"tunnel receive": usageOnly("beam tunnel receive DIRECTORY [options]"),
	"tunnel studio":  studioHelpTopic(),

	"room channel create":      usageOnly("beam room channel create ROOM_ID --name NAME --kind KIND [options]"),
	"room channel publish":     usageOnly("beam room channel publish ROOM_ID CHANNEL_ID (--literal TEXT | --stdin)"),
	"room channel object":      usageOnly("beam room channel object <publish|list|status|cancel> ROOM_ID CHANNEL_ID [options]"),
	"room channel persistence": usageOnly("beam room channel persistence ROOM_ID CHANNEL_ID <status|retry|acknowledge|purge> [options]"),
}

func (a *App) completion(args []string, renderer output.Renderer) error {
	if len(args) == 0 {
		return usage("completion requires install, generate, uninstall, bash, zsh, fish, or powershell")
	}
	if args[0] == "generate" {
		args = args[1:]
	}
	if len(args) == 1 && args[0] == "uninstall" {
		removed, err := uninstallCompletions()
		if err != nil {
			return cliError(ExitOperationFailed, "Could not uninstall Beam completion.", "Remove the completion file manually.", err)
		}
		return renderer.Result(map[string]any{"uninstalled": true, "removed": removed}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Removed %d Beam completion file(s).\n", len(removed))
			return err
		})
	}
	if len(args) >= 1 && args[0] == "install" {
		shell := ""
		if len(args) == 2 {
			shell = args[1]
		} else if len(args) != 1 {
			return usage("completion install accepts at most one shell")
		}
		path, detected, err := installCompletion(shell)
		if err != nil {
			return cliError(ExitOperationFailed, "Could not install Beam completion.", "Use `beam completion generate <shell>` for manual installation.", err)
		}
		return renderer.Result(map[string]any{"installed": true, "shell": detected, "path": path}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Installed %s completion at %s.\n", detected, path)
			return err
		})
	}
	if len(args) != 1 {
		return usage("completion generate requires bash, zsh, fish, or powershell")
	}
	script, ok := completionScript(args[0])
	if !ok {
		return usage("unsupported completion shell " + args[0])
	}
	return renderer.Result(map[string]string{"shell": args[0], "script": script}, func(w io.Writer) error {
		_, err := fmt.Fprint(w, script)
		return err
	})
}

func installCompletion(requested string) (string, string, error) {
	shell := strings.TrimSpace(requested)
	if shell == "" {
		shell = filepath.Base(os.Getenv("SHELL"))
	}
	script, ok := completionScript(shell)
	if !ok {
		return "", shell, fmt.Errorf("unsupported shell %q", shell)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", shell, err
	}
	var path string
	switch shell {
	case "bash":
		path = filepath.Join(home, ".local", "share", "bash-completion", "completions", version.CLIName())
	case "zsh":
		path = filepath.Join(home, ".zsh", "completions", "_"+version.CLIName())
	case "fish":
		path = filepath.Join(home, ".config", "fish", "completions", version.CLIName()+".fish")
	case "powershell":
		path = powerShellProfilePath(home)
		return path, shell, installPowerShellCompletion(path, script)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", shell, err
	}
	return path, shell, os.WriteFile(path, []byte(script), 0o644)
}

func uninstallCompletions() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := []string{
		filepath.Join(home, ".local", "share", "bash-completion", "completions", version.CLIName()),
		filepath.Join(home, ".zsh", "completions", "_"+version.CLIName()),
		filepath.Join(home, ".config", "fish", "completions", version.CLIName()+".fish"),
	}
	var removed []string
	for _, path := range paths {
		if err := os.Remove(path); err == nil {
			removed = append(removed, path)
		} else if !os.IsNotExist(err) {
			return removed, err
		}
	}
	for _, path := range powerShellProfilePaths(home) {
		changed, err := uninstallPowerShellCompletion(path)
		if err != nil {
			return removed, err
		}
		if changed {
			removed = append(removed, path)
		}
	}
	return removed, nil
}

func powerShellProfilePath(home string) string {
	if configured := strings.TrimSpace(os.Getenv("BEAM_POWERSHELL_PROFILE")); configured != "" {
		return configured
	}
	directory := "PowerShell"
	if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(os.Getenv("PSModulePath")), "windowspowershell") {
		directory = "WindowsPowerShell"
	}
	return filepath.Join(home, "Documents", directory, "Microsoft.PowerShell_profile.ps1")
}

func powerShellProfilePaths(home string) []string {
	paths := []string{powerShellProfilePath(home)}
	for _, directory := range []string{"PowerShell", "WindowsPowerShell"} {
		candidate := filepath.Join(home, "Documents", directory, "Microsoft.PowerShell_profile.ps1")
		found := false
		for _, path := range paths {
			if strings.EqualFold(path, candidate) {
				found = true
				break
			}
		}
		if !found {
			paths = append(paths, candidate)
		}
	}
	return paths
}

func powerShellCompletionMarkers() (string, string) {
	name := version.CLIName()
	return "# >>> " + name + " completion >>>", "# <<< " + name + " completion <<<"
}

func installPowerShellCompletion(path, script string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	start, end := powerShellCompletionMarkers()
	content, _ := removeMarkedBlock(string(existing), start, end)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += start + "\n" + strings.TrimRight(script, "\r\n") + "\n" + end + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func uninstallPowerShellCompletion(path string) (bool, error) {
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	start, end := powerShellCompletionMarkers()
	content, changed := removeMarkedBlock(string(existing), start, end)
	if !changed {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(content), 0o644)
}

func removeMarkedBlock(content, start, end string) (string, bool) {
	startIndex := strings.Index(content, start)
	if startIndex < 0 {
		return content, false
	}
	relativeEnd := strings.Index(content[startIndex+len(start):], end)
	if relativeEnd < 0 {
		return content, false
	}
	endIndex := startIndex + len(start) + relativeEnd + len(end)
	for endIndex < len(content) && (content[endIndex] == '\r' || content[endIndex] == '\n') {
		endIndex++
	}
	return content[:startIndex] + content[endIndex:], true
}

func completionScript(shell string) (string, bool) {
	script, ok := rawCompletionScript(shell)
	if !ok {
		return "", false
	}
	cliName := version.CLIName()
	script = strings.ReplaceAll(script, "beam-agentd", version.AgentName())
	switch shell {
	case "bash":
		script = strings.ReplaceAll(script, "# beam bash", "# "+cliName+" bash")
		script = strings.ReplaceAll(script, "complete -F _beam_complete beam", "complete -F _beam_complete "+cliName)
	case "zsh":
		functionName := "_" + strings.ReplaceAll(cliName, "-", "_")
		script = strings.ReplaceAll(script, "#compdef beam", "#compdef "+cliName)
		script = strings.ReplaceAll(script, "_beam", functionName)
		script = strings.ReplaceAll(script, "compdef "+functionName+" beam", "compdef "+functionName+" "+cliName)
	case "fish":
		script = strings.ReplaceAll(script, "-c beam", "-c "+cliName)
	case "powershell":
		script = strings.ReplaceAll(script, "-CommandName beam", "-CommandName "+cliName)
	}
	return script, true
}

func rawCompletionScript(shell string) (string, bool) {
	switch shell {
	case "bash":
		return `# beam bash completion
_beam_complete() {
	local commands="setup status doctor login logout whoami session org context config share receive unshare transfer operation studio logs auth agent action registry room tunnel update version help completion"
  local auth="login logout whoami"
	local agent="connect start stop restart status diagnostics logs rename repair credentials revoke disconnect"
  local action="init inspect validate run pack publish version"
  local registry="search show inspect pack publish versions resolve download verify"
	local room="list inspect create join leave close refresh invite members role channel grant"
	local tunnel="list show stop restart watch prune start status diagnostics studio room expose receive endpoints endpoint close logs upload download bridge duplex identity operations operation cancel"
  if [[ ${COMP_CWORD} -eq 1 ]]; then
    COMPREPLY=( $(compgen -W "${commands}" -- "${COMP_WORDS[COMP_CWORD]}") )
  elif [[ ${COMP_WORDS[1]} == auth && ${COMP_CWORD} -eq 2 ]]; then
    COMPREPLY=( $(compgen -W "${auth}" -- "${COMP_WORDS[COMP_CWORD]}") )
	elif [[ ${COMP_WORDS[1]} == agent && ${COMP_CWORD} -eq 2 ]]; then
		COMPREPLY=( $(compgen -W "${agent}" -- "${COMP_WORDS[COMP_CWORD]}") )
  elif [[ ${COMP_WORDS[1]} == action && ${COMP_CWORD} -eq 2 ]]; then
    COMPREPLY=( $(compgen -W "${action}" -- "${COMP_WORDS[COMP_CWORD]}") )
  elif [[ ${COMP_WORDS[1]} == registry && ${COMP_CWORD} -eq 2 ]]; then
    COMPREPLY=( $(compgen -W "${registry}" -- "${COMP_WORDS[COMP_CWORD]}") )
	elif [[ ${COMP_WORDS[1]} == room && ${COMP_CWORD} -eq 2 ]]; then
		COMPREPLY=( $(compgen -W "${room}" -- "${COMP_WORDS[COMP_CWORD]}") )
  elif [[ ${COMP_WORDS[1]} == tunnel && ${COMP_CWORD} -eq 2 ]]; then
    COMPREPLY=( $(compgen -W "${tunnel}" -- "${COMP_WORDS[COMP_CWORD]}") )
  fi
}
complete -F _beam_complete beam
`, true
	case "zsh":
		return `#compdef beam
_beam() {
  if (( CURRENT == 3 )) && [[ ${words[2]} == room ]]; then
    _values 'room command' list inspect create join leave close refresh invite members role channel grant
    return
  fi
  if (( CURRENT == 4 )) && [[ ${words[2]} == tunnel && ${words[3]} == room ]]; then
    _values 'room command' list inspect create join leave close refresh invite members role channel grant
    return
  fi
  if (( CURRENT == 3 )) && [[ ${words[2]} == tunnel ]]; then
    _values 'tunnel command' list show stop restart watch prune start status diagnostics studio room expose receive endpoints endpoint close logs upload download bridge duplex identity operations operation cancel
    return
  fi
  local -a commands
	commands=(
		'setup:complete Beam onboarding'
		'status:show Beam status'
		'doctor:diagnose Beam'
		'login:sign in to Beam'
		'logout:sign out of Beam'
		'whoami:show the Beam identity'
		'session:inspect the OAuth session'
		'org:select an organization'
		'context:select a Beam context'
		'config:manage Beam configuration'
		'share:share a file port or URL'
		'receive:create a receive destination'
		'unshare:close a share'
		'transfer:manage transfers'
		'operation:manage durable operations'
		'studio:connect Beam Studio'
		'logs:show daemon logs'
		'auth:manage the central Beam CLI identity'
		'agent:register and operate this machine identity'
    'action:build and run Beam action packages locally'
    'registry:interact with beam-registry'
    'room:create and operate Beam rooms through beam-agentd'
    'tunnel:manage endpoints and transfers through beam-agentd'
    'update:update beam and beam-agentd together'
    'version:show version'
    'help:show help'
    'completion:generate completion'
  )
  _describe 'command' commands
}
compdef _beam beam
`, true
	case "fish":
		return `complete -c beam -f
complete -c beam -n '__fish_use_subcommand' -a 'setup status doctor login logout whoami session org context config share receive unshare transfer operation studio logs'
complete -c beam -n '__fish_use_subcommand' -a auth -d 'Manage the central Beam CLI identity'
complete -c beam -n '__fish_use_subcommand' -a agent -d 'Register and operate this machine identity'
complete -c beam -n '__fish_use_subcommand' -a action -d 'Build and run Beam action packages locally'
complete -c beam -n '__fish_use_subcommand' -a registry -d 'Interact with beam-registry'
complete -c beam -n '__fish_use_subcommand' -a room -d 'Create and operate Beam rooms through beam-agentd'
complete -c beam -n '__fish_use_subcommand' -a tunnel -d 'Manage endpoints and transfers through beam-agentd'
complete -c beam -n '__fish_use_subcommand' -a update -d 'Update beam and beam-agentd together'
complete -c beam -n '__fish_use_subcommand' -a version -d 'Show version'
complete -c beam -n '__fish_use_subcommand' -a help -d 'Show help'
complete -c beam -n '__fish_use_subcommand' -a completion -d 'Generate completion'
complete -c beam -n '__fish_seen_subcommand_from auth' -a 'login logout whoami'
complete -c beam -n '__fish_seen_subcommand_from agent' -a 'connect start stop restart status diagnostics logs rename repair credentials revoke disconnect'
complete -c beam -n '__fish_seen_subcommand_from action' -a 'init inspect validate run pack publish version'
complete -c beam -n '__fish_seen_subcommand_from registry' -a 'search show inspect pack publish versions resolve download verify'
complete -c beam -n '__fish_seen_subcommand_from room' -a 'list inspect create join leave close refresh invite members role channel grant'
complete -c beam -n '__fish_seen_subcommand_from tunnel' -a 'list show stop restart watch prune start status diagnostics studio room expose receive endpoints endpoint close logs upload download bridge duplex identity operations operation cancel'
`, true
	case "powershell":
		return `Register-ArgumentCompleter -CommandName beam -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $elements = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })
    $candidates = if ($elements.Count -le 2) {
        @('setup', 'status', 'doctor', 'login', 'logout', 'whoami', 'session', 'org', 'context', 'config', 'share', 'receive', 'unshare', 'transfer', 'operation', 'studio', 'logs', 'auth', 'agent', 'action', 'registry', 'room', 'tunnel', 'update', 'version', 'help', 'completion')
    } elseif ($elements[1] -eq 'agent' -and $elements.Count -le 3) {
        @('connect', 'start', 'stop', 'restart', 'status', 'diagnostics', 'logs', 'rename', 'repair', 'credentials', 'revoke', 'disconnect')
    } elseif ($elements[1] -eq 'room' -and $elements.Count -le 3) {
        @('list', 'inspect', 'create', 'join', 'leave', 'close', 'refresh', 'invite', 'members', 'role', 'channel', 'grant')
    } elseif ($elements[1] -eq 'tunnel' -and $elements.Count -le 3) {
        @('list', 'show', 'stop', 'restart', 'watch', 'prune', 'start', 'status', 'diagnostics', 'studio', 'room', 'expose', 'receive', 'endpoints', 'endpoint', 'close', 'logs', 'upload', 'download', 'bridge', 'duplex', 'identity', 'operations', 'operation', 'cancel')
    } else {
        @()
    }
    $candidates |
        Where-Object { $_ -like "$wordToComplete*" } |
        ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}
`, true
	default:
		return "", false
	}
}

func applyReleaseNames(text string) string {
	text = strings.ReplaceAll(text, "beam-agentd", version.AgentName())
	cliName := version.CLIName()
	// A help topic's usage line is stored without its "Usage:" label, because the
	// renderer styles the label separately. That leaves the CLI name at position
	// zero, where none of the prefixes below can match it.
	if strings.HasPrefix(text, "beam ") {
		text = cliName + strings.TrimPrefix(text, "beam")
	}
	for _, prefix := range []string{"Usage: beam ", "`beam ", `"beam `, "\nbeam ", "\n  beam ", "Run \"beam ", "Next: beam "} {
		text = strings.ReplaceAll(text, prefix, strings.Replace(prefix, "beam", cliName, 1))
	}
	text = strings.ReplaceAll(text, "after beam exits", "after "+cliName+" exits")
	text = strings.ReplaceAll(text, "current beam process", "current "+cliName+" process")
	text = strings.ReplaceAll(text, "Update beam and", "Update "+cliName+" and")
	text = strings.ReplaceAll(text, "update beam and", "update "+cliName+" and")
	text = strings.ReplaceAll(text, "matching beam and", "matching "+cliName+" and")
	return text
}
