package command

// The help text is data rather than pre-formatted strings so that one source
// can render two ways: boxed panels on an interactive terminal, and the plain
// indented form everywhere else, including the text embedded in --json.
//
// A helpEntry with an empty Desc is a bare syntax line. The room and tunnel
// topics are mostly these: the command shape is the documentation, and inventing
// a description for every spelling would pad the page without adding meaning.

type helpEntry struct {
	Name string
	Desc string
}

type helpSection struct {
	Title   string
	Entries []helpEntry
}

type helpTopic struct {
	// Banner shows the wordmark. Only the root topic sets it; a subcommand's
	// help is something you read mid-task, not an arrival.
	Banner   bool
	Summary  string
	Usage    []string
	Sections []helpSection
	// Notes are trailing prose paragraphs. They stay outside panels because they
	// are sentences, not scannable rows.
	Notes []string
	// Footer is the closing pointer line, kept separate so it can be styled.
	Footer string
}

func line(name string) helpEntry { return helpEntry{Name: name} }

func lines(names ...string) []helpEntry {
	entries := make([]helpEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, line(name))
	}
	return entries
}

var rootHelpTopic = helpTopic{
	Banner:  true,
	Summary: "The unified command-line client for Beam.",
	Usage:   []string{"beam [--json | --quiet] <command> [options]"},
	Sections: []helpSection{
		{Title: "Getting started", Entries: []helpEntry{
			{"setup", "Complete login, organization, agent, and daemon onboarding"},
			{"status", "Show the current Beam account, agent, daemon, and resources"},
			{"doctor", "Diagnose configuration, authentication, daemon, and versions"},
		}},
		{Title: "Identity and account", Entries: []helpEntry{
			{"login", "Sign in to Beam"},
			{"logout", "Sign out and remove local credentials"},
			{"whoami", "Show the active Beam identity"},
			{"session", "Inspect or refresh the OAuth session"},
			{"org", "List and select Beam organizations"},
			{"auth", "Manage the central Beam CLI identity"},
		}},
		{Title: "Share and transfer", Entries: []helpEntry{
			{"share", "Share a file, port, or URL"},
			{"receive", "Create a public receive destination"},
			{"unshare", "Close a named or recent share"},
			{"transfer", "Create and inspect file transfers"},
			{"operation", "Inspect, watch, retry, or cancel durable operations"},
			{"budget", "Show monthly budget threshold alerts"},
		}},
		{Title: "Rooms and tunnels", Entries: []helpEntry{
			{"room", "Create and operate Beam rooms through beam-agentd"},
			{"tunnel", "Manage endpoints and transfers through beam-agentd"},
			{"studio", "Connect this machine to Beam Studio"},
		}},
		{Title: "This machine", Entries: []helpEntry{
			{"agent", "Register and operate this machine identity"},
			{"logs", "Show beam-agentd logs"},
			{"context", "Create and select local/dev/prod contexts"},
			{"config", "Inspect and edit Beam configuration"},
		}},
		{Title: "Actions and registry", Entries: []helpEntry{
			{"action", "Build and run Beam action packages locally"},
			{"registry", "Interact with beam-registry over HTTPS"},
		}},
		{Title: "About the CLI", Entries: []helpEntry{
			{"update", "Update beam and beam-agentd together"},
			{"version", "Show build version"},
			{"help", "Show help"},
			{"completion", "Generate shell completion for bash, zsh, fish, or PowerShell"},
		}},
		{Title: "Global options", Entries: []helpEntry{
			{"--json", "Emit machine-readable JSON"},
			{"--quiet, -q", "Suppress successful output"},
			{"--context, -c", "Select a named Beam context"},
			{"--no-interactive", "Disable prompts and browser interaction"},
			{"--verbose, -v", "Enable detailed diagnostics"},
		}},
	},
	Footer: `Run "beam help <command>" for details.`,
}

var helpTopics = map[string]helpTopic{
	"budget": {
		Usage: []string{"beam budget [options]"},
		Sections: []helpSection{
			{Title: "Options", Entries: []helpEntry{
				{"--unacknowledged", "Only alerts not yet dismissed in the console"},
				{"--limit <n>", "How many to show, 1-200 (default 50)"},
			}},
		},
		Notes: []string{
			"Lists monthly budget threshold crossings for the selected organization. Beam records one alert per target, month and threshold, so this is a history of crossings rather than a line for every transfer.",
			"A budget with \"block when exceeded\" set refuses usage once it is spent. That check runs when usage is recorded, so it stops the next charge rather than one already in flight.",
			"Budgets and thresholds are set per API key or per project in the console.",
		},
	},

	"auth": {
		Usage: []string{"beam auth <command>"},
		Sections: []helpSection{
			{Title: "Authentication commands", Entries: []helpEntry{
				{"login", "Sign in with OAuth Device Authorization Grant"},
				{"logout", "Revoke the refresh token and remove local credentials"},
				{"whoami", "Refresh and show the Beam API identity and organizations"},
			}},
		},
		Notes: []string{
			"Beam CLI is the public OAuth client beam-cli with scope cli:access. It never embeds a client secret. Service commands consume this central authentication but do not own login, logout, or identity.",
		},
	},

	"action": {
		Usage: []string{"beam action <command> [options]"},
		Sections: []helpSection{
			{Title: "Action commands", Entries: []helpEntry{
				{"init [directory]", "Create a Beam Action package"},
				{"inspect [directory]", "Show package metadata and files"},
				{"validate [directory]", "Validate manifest and entrypoint"},
				{"run [directory] [options]", "Build and run a Node action package locally"},
				{"pack [directory] [--out FILE]", "Create a deterministic archive in dist/"},
				{"publish [directory] [--tag TAG]", "Validate, pack, and publish (default latest)"},
				{"version [directory]", "Compare local and published versions"},
			}},
			{Title: "Run options", Entries: []helpEntry{
				{"--config <file>", "JSON action configuration"},
				{"--inputs <file>", "JSON action inputs"},
				{"--secrets <file>", "JSON secrets available through context.secrets"},
				{"--log-level <level>", "debug, info, warn, or error (default info)"},
				{"--runtime <mode>", "mock or live (default mock)"},
			}},
		},
		Notes: []string{
			"When omitted, fixture files default to fixtures/config.local.json, fixtures/input.local.json, and fixtures/secrets.local.json. Missing default files are treated as empty JSON objects.",
		},
	},

	"agent": {
		Usage: []string{"beam agent <command> [options]"},
		Sections: []helpSection{
			{Title: "Agent commands", Entries: []helpEntry{
				{"connect [options]", "Register this machine with the coordinator"},
				{"start", "Start beam-agentd"},
				{"stop", "Stop beam-agentd"},
				{"restart", "Restart beam-agentd"},
				{"status", "Show the canonical agent identity and connection"},
				{"dashboard [--no-open]", "Start the temporary local management dashboard"},
				{"diagnostics", "Show daemon, identity, heartbeat, and runtime health"},
				{"logs [--follow]", "Show daemon logs"},
				{"repair", "Recover the local agent runtime"},
				{"disconnect", "Forget the local enrollment and stop beam-agentd"},
				{"rename <label>", "Change the human-readable label; the agent ID stays stable"},
				{"credentials recover", "Rotate a lost or rejected API key using the machine key"},
				{"revoke --yes", "Revoke this machine and its active credentials"},
			}},
			{Title: "Connect options", Entries: []helpEntry{
				{"--coordinator <https-url>", "Coordinator URL (or BEAM_COORDINATOR_URL)"},
				{"--organization <id-or-slug>", "Beam organization; the default is selected automatically"},
				{"--label <name>", "Human-readable machine label (default: hostname)"},
				{"--enrollment-token <token>", "Non-interactive one-time enrollment token"},
			}},
		},
		Notes: []string{
			"The daemon creates one durable Ed25519 machine identity. Interactive connect starts OAuth device login when needed, requests a short-lived enrollment from the coordinator, and stores the returned runtime credential in owner-only daemon state. Secret values are never printed. Coordinator URLs must use HTTPS outside loopback.",
		},
	},

	"registry": {
		Usage: []string{"beam registry <command> [options]"},
		Sections: []helpSection{
			{Title: "Registry commands", Entries: []helpEntry{
				{"search [query]", "Search published packages"},
				{"show <package>", "Show package metadata"},
				{"inspect [directory]", "Validate and inspect an action package"},
				{"pack [directory] --out <file>", "Create a deterministic package archive"},
				{"publish [directory] --tag <tag>", "Pack and publish an action version"},
				{"versions <package>", "List package versions"},
				{"resolve <package> [--range <range>]", "Resolve a tag or semantic version range"},
				{"download <package>@<version>", "Download and verify an artifact"},
				{"verify <archive>", "Verify a local Beam Action archive"},
			}},
		},
	},

	"room": {
		Usage: []string{"beam room <command> [options]"},
		Sections: []helpSection{
			{Title: "Room commands", Entries: lines(
				"<room-id> show|refresh|watch|leave|close",
				"<room-id> invite [--out <file>]",
				"<room-id> invitation <list|show|revoke>",
				"<room-id> member <list|show|watch|remove>",
				"<room-id> role <list|show|create|assign|revoke|delete>",
				"<room-id> channel <list|create>",
				"<room-id> channel <channel-id> <show|update|close|publish|listen>",
				"<room-id> channel <channel-id> <stream|datagram|object|media|persistence|grant> ...",
			)},
			{Title: "Media lifecycle", Entries: lines(
				"<room-id> channel <channel-id> media stop <workload-id>",
			)},
			{Title: "Compatibility commands", Entries: lines(
				"list [--state <active|closed|all>]",
				"inspect <room-id>",
				"create [--organization <id|slug>] [--lease-ttl <seconds>] [--idempotency-key <key>]",
				"join <room-id> (--invitation-token <token> | --invitation-file <file>)",
				"leave <room-id>",
				"close <room-id>",
				"refresh <room-id>",
				"invite <room-id> [--role <role-id>] [--channel-access <channel-id>=<action,...>] [options]",
				"members <room-id>",
				"role <list|create|assign|revoke> <room-id> [options]",
				"channel <command> <room-id> [options]",
				"channel object publish <room-id> <channel-id> (--file <path> | --from <member-id> --object-key <key>) [--to <member-id>] [--allow-partial] [--follow] [--interval <duration>]",
				"channel object status <room-id> <channel-id> <publication-id> [--follow] [--interval <duration>]",
				"channel object <list|cancel> <room-id> <channel-id> [publication-id]",
				"channel media <publish|view> <room-id> <channel-id>",
				"grant <list|put|revoke> <room-id> <channel-id> [options]",
			)},
		},
		Notes: []string{
			"Room commands call beam-agentd directly through its owner-only local socket. The former \"beam tunnel room ...\" namespace remains available as a compatibility alias and has identical behavior.",
		},
	},

	"tunnel": {
		Usage: []string{"beam tunnel <command> [options]"},
		Sections: []helpSection{
			{Title: "Tunnel commands", Entries: append(lines(
				"list",
				"show <name-or-id>",
				"stop <name-or-id>",
				"restart <name-or-id>",
				"watch <name-or-id>",
				"prune",
				"start [--coordinator <url>] [--transport <auto|quic|v1|v0>]",
				"stop",
				"status",
				"diagnostics",
				"studio <connect|status|permissions|update-permissions|disconnect> [options]",
			), append([]helpEntry{{"room <command> [options]", `Compatibility alias for "beam room"`}}, lines(
				"expose file <path> [public options]",
				"expose http <port-or-url> [public options]",
				"expose stream <port-or-url> [public options]",
				"expose webrtc <port-or-url> [public options]",
				"expose tcp <port-or-address> [--relay-id <id>]",
				"receive <directory> [public options]",
				"endpoints",
				"endpoint <id>",
				"close <id>",
				"logs [--follow]",
				"upload <file> (--session <file> | --workload-id <id>) [transfer options]",
				"download <file> (--session <file> | --workload-id <id>) [transfer options]",
				"bridge <lease> --identity-key <file> [--listen <addr> | --target <addr>]",
				"bridge sign <intent> --identity-key <file> --agent-id <id>",
				"duplex <file>",
				"identity generate --identity-key <file> --agent-id <id>",
				"operations",
				"operation <id>",
				"cancel <id>",
			)...)...)},
			{Title: "Public options", Entries: []helpEntry{
				{"--no-public", "Use the legacy relay URL instead"},
				{"--public-endpoint-key <key>", "Stable key used to resume the public URL"},
				{"--standbys <0-3>", "Number of standby relay sessions (default 1)"},
				{"--relay-id <id>", "Pin the primary relay"},
				{"--shutdown-grace <duration>", "In-flight drain window (default 5s)"},
				{"--object-storage", "Use the SigV4 S3-compatible profile (file/receive)"},
				{"--object-storage-config <path>", "Write provider JSON with owner-only permissions"},
			}},
			{Title: "Transfer options", Entries: []helpEntry{
				{"--manifest <file>", "Download range manifest"},
				{"--receipts-out <file>", "Download receipt batch output"},
				{"--filename <name>", "Destination filename for upload"},
				{"--upload-id <id>", "Resume a multipart upload"},
				{"--plan-version <n>", "Known coordinator plan version"},
				{"--min-relays <n>", "Required relay candidates (default 1)"},
				{"--standbys <n>", "Standbys per chunk or part (default 1)"},
				{"--concurrency <n>", "Parallel chunks or parts (default 4)"},
				{"--part-size <bytes>", "Multipart upload part size"},
				{"--transport <auto|quic|v1|v0>", "Advanced transport policy"},
			}},
		},
		Notes: []string{
			"All commands use the private local API of the companion beam-agentd daemon. Room commands call beam-agentd directly over the same owner-only local daemon transport as every other tunnel command.",
			"Long-running transfers and bridges continue after beam exits and can be inspected or cancelled by operation ID.",
			"File, HTTP, stream, WebRTC, and receive endpoints are public by default and return a stable https://<public-id>.tunnel.b3m.dev URL.",
		},
	},

	"update": {
		Usage:   []string{"beam update [options]"},
		Summary: "Update beam and its matching beam-agentd companion from the Beam CDN.",
		Sections: []helpSection{
			{Title: "Options", Entries: []helpEntry{
				{"--check", "Check whether a newer bundle is available"},
				{"--version <version>", "Install a specific published version"},
				{"--force", "Reinstall the selected version"},
			}},
		},
		Notes: []string{
			"The command verifies the archive SHA-256 checksum before stopping beam-agentd and replacing both binaries. Set BEAM_CDN_BASE_URL only for a trusted technical mirror. On Windows, replacement finishes after the current beam process exits.",
		},
	},
}
