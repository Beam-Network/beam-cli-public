# beam

`beam` is the fast, unified Beam client:

```console
beam auth ...
beam action ...
beam registry ...
beam room ...
```

Only Rooms are available for now. Tunnel commands (`share`, `receive`,
`unshare`, `transfer`, `operation`, and `tunnel`) are hidden from help and
completion and refuse to run; `BEAM_EXPERIMENTAL_TUNNELS=1` re-enables them for
internal testing.

It is intentionally a thin client. The binary contains no Registry server,
Tunnel network stack, transfer engine, or daemon. Registry commands call the
public Beam Registry API at `https://api.b1m.ai/registry`. Room commands call
the versioned local API of the companion tunnel agent that ships in every
release bundle.

## Installation

Release archives are designed for macOS (`arm64`, `amd64`), Linux (`arm64`,
`amd64`), and Windows (`amd64`). Every archive contains matching CLI and
tunnel-agent binaries plus SHA-256 verification. Compiled release bundles are
distributed from `cdn.b1m.ai`:

```console
curl -fsSL https://cdn.b1m.ai/cli/install.sh | sh
```

That one-liner installs `beam` from the production channel.

On a first interactive installation, the installer launches `beam setup`.
The onboarding can connect a Beam account, select an organization, name the
machine, join a Room from an invitation, and install shell completion. Set
`BEAM_SKIP_ONBOARDING=1` for unattended installations.

Windows uses `packaging/install.ps1`. See
[`docs/installing.md`](docs/installing.md) for update and uninstall behavior.

The coordinated bundle includes the managed-agent backend for Studio workflow
actions. Execution requires explicit local opt-in, an action allowlist, and a
separately installed Node.js 22+ shared action runtime with its native process
guard. Installing or updating Beam does not enable action execution.

Release bundles use the `*.b1m.ai` authentication, API, Registry, and
coordinator services, and update from `cdn.b1m.ai`. The corresponding
`BEAM_*_URL` variables can override the compiled-in defaults for private
environments.

### Building from source

```console
make build
./beam version
```

`make build` produces a production-shaped `beam`. The `BEAM_VERSION` and
`BEAM_*_URL` build variables compile in another release channel or other
service endpoints. The tunnel agent is not built from
this repository: a source-built `beam` uses the agent installed beside it, or
the one named by `BEAM_TUNNEL_AGENT_BINARY`.

## Main commands

The task-oriented entry points cover onboarding, diagnostics, contexts, and
configuration:

```text
beam setup
beam status
beam doctor [--fix]
beam login | beam logout | beam whoami
beam context <list|current|create|use|show|delete>
beam config <path|list|get|set|unset|edit|validate>
beam studio <command>
beam agent disconnect
```

`beam setup` is the cross-platform onboarding wizard. In a terminal it
offers account enrollment, invitation-based Room enrollment, or deferring
configuration. For automation, use `--mode account|room|skip` with the matching
options and `--no-interactive`. The mode is mandatory for non-interactive setup.
`beam doctor` recognizes connected invitation-only Room identities without
requiring account login; account-mode and invalid-credential checks remain active.
The wizard is resumable, preserves a successful login when no organization is
available, rejects restricted organizations, and never silently moves an
already registered agent to another organization or coordinator.

See [`docs/command-reference.md`](docs/command-reference.md) for the complete
resource-scoped command tree and compatibility spellings.

```text
beam version
beam update
beam update --check
beam help
beam completion bash|zsh|fish|powershell

beam auth login
beam auth logout
beam auth whoami

beam agent connect [--coordinator <https-url>] [--api-key <key>]
beam agent status
beam agent diagnostics
beam agent rename <label>
beam agent credentials recover
beam agent revoke --yes

beam action run [directory] \
  --config <file> --inputs <file> --secrets <file>

beam registry inspect [directory]
beam registry pack [directory] --out <file>
beam registry publish [directory] --tag <tag>
beam registry versions <package>
beam registry resolve <package> [--range <range>]

beam room create [--api-key <key>]
beam room list [--state <active|closed|all>]
beam room inspect <room-id>
beam room invite <room-id> [options]
beam room <room-id> invitation <list|show|revoke> [invitation-id]
beam room join <room-id> (--invitation-token <token> | --invitation-file <file>)
beam room members <room-id>
beam room <room-id> member remove <member-id>
beam room role <list|create|assign|revoke|delete> <room-id> [options]
beam room channel <command> <room-id> [options]
beam room grant <list|put|revoke> <room-id> <channel-id> [options]
beam room leave <room-id>
beam room close <room-id>
```

`beam agent connect` is the account-enrollment entry point used by setup and
starts the tunnel agent automatically. It opens OAuth device login when the
CLI has no Beam session, selects the default organization, creates the durable
machine identity, and registers it with the configured coordinator. Explicit
`beam agent start`, `beam agent stop`, and `beam agent restart` remain available
for service management. Restart and bundle replacement wait for the old daemon
to release its local socket before starting a replacement. Commands that need
the daemon start it automatically; `beam agent status` only reports, so it
shows a stopped agent as stopped.

Headless machines (CI runners, servers) can enroll without a Beam login by
passing an organization Beam API key, either with `--api-key` or through
`BEAM_API_KEY`:

```console
BEAM_API_KEY=... beam agent connect --label ci-runner
```

The CLI uses the key to mint the one-time enrollment for this machine; the key
itself is never stored or printed, and it is removed from the environment of
any agent daemon the command starts. The key's organization is used;
`--organization`, if given, must name that same organization by ID (a slug or
public ID is recognised only after `beam login` on this machine). The key must
be active and its role must grant at least one billable action (`rooms:start`,
`rooms:create`, `transfers:create`, `workflows:run`, or `actions:execute`);
remaining credit is not needed to enroll. Grant `rooms:start` if the machine will also create Rooms.
`beam setup` keeps its account flow and ignores `BEAM_API_KEY`.

Joining a Room is the intentional exception: `beam room join` can bootstrap a
fresh machine identity directly from an unbound Room invitation. The joining
machine needs no Beam account or OAuth session. Creating Rooms and invitations
still requires an enrolled owner/admin identity.

Rooms use the daemon's private socket and identity:

```text
beam room ... -> tunnel agent -> coordinator
```

`beam room` is the canonical namespace. See [`docs/rooms.md`](docs/rooms.md) for onboarding,
invitations, roles, channels, grants, and automation examples.

`beam update` resolves the latest bundle from the release CDN (`cdn.b1m.ai`),
verifies its published SHA-256 checksum, stops the daemon only after
verification, and replaces `beam` and the tunnel agent together. Use
`beam update --check` without changing the installation, `beam update --version
v1.2.3` to select a published version, or `beam update --force` to repair an
installation at the current version. On Windows, the final replacement runs
after the current CLI process exits because Windows locks a running executable.

Node action packages can be built and executed locally with the same action
context shape used by the Registry development runner:

```console
beam action run . \
  --config fixtures/config.json \
  --inputs fixtures/input.json \
  --secrets fixtures/secrets.json
```

The command runs the package build script when one is declared, loads the
manifest entrypoint, and makes the secrets file available through
`context.secrets`. Node.js and any build tools declared by the action package
must be installed locally. Secrets are passed to the runner over stdin rather
than command-line arguments.

## Architecture and ownership

| Component | Owns | Does not live in `beam` |
| --- | --- | --- |
| `beam` | OAuth Device Grant session, argument parsing, validation, local action execution, Room and Tunnel presentation, config, output, daemon lifecycle, local API clients | auth server, Beam API, Registry server, room state, transfer state, Tunnel networking |
| Beam Auth | central user authentication and token issuance | CLI credential storage and presentation |
| Beam API | RS256 bearer validation, profile, and organization context | OAuth issuance and local credential storage |
| Beam Registry API | Registry HTTP contract, manifests, publishing policy, package data | login, logout, CLI identity presentation |
| tunnel agent | local API server, identity, network protocols, endpoints, long operations, resume, receipts, worker supervision | public command parsing and presentation |

Operator binaries such as `beam-coordinator`, `beam-relay`, `beam-validator`,
and `beam-dashboard` remain separate. See
[`docs/architecture.md`](docs/architecture.md),
[`docs/authentication.md`](docs/authentication.md), and
[`docs/daemon-local-api.md`](docs/daemon-local-api.md), and
[`docs/rooms.md`](docs/rooms.md). Direct invocation of the tunnel agent is
not a supported workflow.

## Configuration

Each release channel owns its own state namespace, so bundles from different
channels coexist on one machine without sharing configuration, credentials or
daemon state. On macOS and Linux, production uses:

```text
~/.config/beam/config.json
~/.config/beam/credentials.json
~/.beam/agent.sock
```

`XDG_CONFIG_HOME` is honored. Windows uses the standard user configuration
directory and the named pipe `\\.\pipe\beam-agent`.

Example production `config.json`:

```json
{
  "registry_url": "https://api.b1m.ai/registry",
  "auth_url": "https://auth.b1m.ai",
  "api_url": "https://api.b1m.ai",
  "coordinator_url": "https://coordinator.example.com",
  "output": "human"
}
```

The rotating refresh token is stored in the OS credential store when available
(Keychain on macOS, Credential Manager on Windows, Secret Service on Linux).
The fallback is `credentials.json` with an atomic write and restrictive
permissions (`0600` inside a `0700` directory on Unix). Access tokens are kept
in process memory only. Phase 1 access-token credentials are deleted and users
must sign in again. Tokens are never printed or placed in repository files.

Technical overrides:

| Variable | Purpose |
| --- | --- |
| `BEAM_REGISTRY_URL` | Registry API base URL |
| `BEAM_AUTH_URL` | authentication service base URL |
| `BEAM_API_URL` | central Beam API base URL |
| `BEAM_CONSOLE_URL` | Beam Console base URL used to resume organization onboarding |
| `BEAM_COORDINATOR_URL` | agent identity and runtime coordinator base URL; HTTPS is required outside loopback |
| `BEAM_ORGANIZATION` | active Beam organization ID, public ID, or slug |
| `BEAM_CONTEXT` | named configuration context to load after the base config |
| `BEAM_AGENT_SOCKET` | Unix socket or Windows named pipe |
| `BEAM_TUNNEL_AGENT_BINARY` | companion tunnel-agent binary override used by `beam agent start` |
| `BEAM_AGENTD_BINARY` | deprecated compatibility alias for `BEAM_TUNNEL_AGENT_BINARY` |
| `BEAM_AGENT_LOG` | daemon log path override |
| `BEAM_EXPERIMENTAL_TUNNELS` | set to `1` to re-enable the hidden tunnel commands for internal testing |
| `BEAM_CONFIG_DIR` | configuration and credentials directory |
| `BEAM_CDN_BASE_URL` | release CDN base URL used by the installers and `beam update`; defaults to the bundle's own channel |
| `BEAM_OUTPUT` | `human`, `json`, or `quiet` |
| `BEAM_SKIP_ONBOARDING` | skip the first-install interactive onboarding when set to `1`, `true`, or `yes` |
| `BEAM_POWERSHELL_PROFILE` | PowerShell profile path supplied by the Windows installer for completion setup |

Release bundles compile in the service endpoints of their channel; a source
build uses the production defaults unless the `BEAM_*_URL` build variables are
set. Use `BEAM_COORDINATOR_URL` or `--coordinator` only to select a private
control plane explicitly.

## Output

Human-readable output is the default. Use global `--json` anywhere in a
command for automation:

```console
beam registry versions @beam/example --json
beam room list --json
```

Successful non-streaming commands write one JSON value to stdout and no
progress text. Errors are structured JSON on stderr. `beam logs
--follow --json` uses newline-delimited JSON (one valid event object per line).
Use `--quiet` or `-q` to suppress successful output.

## Stable exit codes

| Code | Meaning |
| ---: | --- |
| 0 | success |
| 2 | invalid command or argument |
| 3 | invalid configuration or credentials storage |
| 4 | missing or expired authentication |
| 5 | Registry unavailable |
| 6 | daemon unavailable |
| 7 | daemon/CLI protocol incompatible |
| 8 | resource not found |
| 9 | state or immutable-version conflict |
| 10 | operation failed |
| 130 | interrupted |

## Local development

`.env.example` is the versioned, secret-free template for local technical
overrides (`BEAM_*_URL`, `BEAM_AGENT_SOCKET`, `BEAM_OUTPUT`). Copies such as
`.env.local` are ignored by Git and must contain technical overrides only —
never tokens, passwords, API keys, or other secrets. The CLI itself has no
profile flags; load a profile with your shell or process manager.

Validation:

```console
make check
make build
```

Tests use `httptest` and fake local socket servers. They do not contact real
Beam services, open browsers, or modify real credentials.

## Dependencies

Runtime code uses `github.com/zalando/go-keyring` for native credential stores
and `github.com/Microsoft/go-winio` for Windows named pipes. The fallback
credential file and all refresh-token rotations use atomic replacement.

## License

MIT
