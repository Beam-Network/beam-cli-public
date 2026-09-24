# beam

`beam` is the fast, unified Beam client:

```console
beam auth ...
beam action ...
beam registry ...
beam room ...
beam tunnel ...
```

It is intentionally a thin client. The binary contains no Registry server,
Tunnel network stack, transfer engine, or daemon. Registry commands call the
public HTTP API owned by
`beam-website` at `https://api.b1m.ai/registry`. Tunnel
commands call the versioned local API owned by
`beam-tunnel-agent`.

## Installation

Build from source with:

```console
make build
./beam version
```

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
guard. Installing or updating Beam does not enable action execution. See the
managed-agent execution instructions.

Builds use the `*.b1m.ai` authentication, API, Registry and coordinator
services, and update from `cdn.b1m.ai`. The corresponding `BEAM_*_URL`
variables can override the compiled-in defaults for private environments.

## Main commands

The task-oriented entry points cover onboarding, diagnostics, contexts, and
common sharing workflows:

```text
beam setup
beam status
beam doctor [--fix]
beam login | beam logout | beam whoami
beam context <list|current|create|use|show|delete>
beam config <path|list|get|set|unset|edit|validate>
beam share <file|port|url> [--name <name>]
beam receive <directory> [--name <name>]
beam unshare <name|id|--last>
beam transfer <command>
beam operation <command>
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

beam agent connect [--coordinator <https-url>]
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

beam tunnel start [--coordinator <url>]
beam tunnel stop
beam tunnel status
beam tunnel diagnostics
beam tunnel expose file <path> [public options]
beam tunnel expose http <port-or-url> [public options]
beam tunnel expose stream <port-or-url> [public options]
beam tunnel expose webrtc <port-or-url> [public options]
beam tunnel expose tcp <port-or-address> [--relay-id <id>]
beam tunnel receive <directory> [public options]
beam tunnel endpoints
beam tunnel endpoint <id>
beam tunnel close <id>
beam tunnel logs [--follow]
beam tunnel upload <file> (--session <file> | --workload-id <id>)
beam tunnel download <file> (--session <file> | --workload-id <id>)
beam tunnel bridge <lease> --identity-key <file>
beam tunnel bridge sign <intent> --identity-key <file> --agent-id <id>
beam tunnel duplex <file>
beam tunnel identity generate --identity-key <file> --agent-id <id>
beam tunnel operations
beam tunnel operation <id>
beam tunnel cancel <id>
```

`beam agent connect` is the account-enrollment entry point used by setup and
starts `beam-tunnel-agent` automatically. It opens OAuth device login when the
CLI has no Beam session, selects the default organization, creates the durable
machine identity, and registers it with the configured coordinator. Explicit
`beam agent start`, `beam agent stop`, and `beam agent restart` remain available
for service management. Restart and bundle replacement wait for the old daemon
to release its local socket before starting a replacement. Commands that need
the daemon start it automatically. Long transfers and bridges continue inside
the daemon after `beam` exits; their durable operation ID is used for inspection
and cancellation.

Joining a Room is the intentional exception: `beam room join` can bootstrap a
fresh machine identity directly from an unbound Room invitation. The joining
machine needs no Beam account or OAuth session. Creating Rooms and invitations
still requires an enrolled owner/admin identity.

Rooms use the same private socket and daemon identity as tunnel operations:

```text
beam room ... -> beam-tunnel-agent -> coordinator
```

`beam room` is the canonical namespace. `beam tunnel room` remains an exact
compatibility alias for existing scripts; it does not launch another binary or
use a separate identity. See [`docs/rooms.md`](docs/rooms.md) for onboarding,
invitations, roles, channels, grants, and automation examples.

`beam update` resolves the latest bundle from its own channel CDN
(`cdn.b1m.ai` in production, `cdn.b1m.ai` in development), verifies its
published SHA-256 checksum, stops the daemon only after verification, and
replaces `beam` and `beam-tunnel-agent` together. Use `beam update --check` without
changing the installation, `beam update --version v1.2.3` to select a published
version, or `beam update --force` to repair an installation at the current
version. On Windows, the final replacement runs after the current CLI process
exits because Windows locks a running executable.

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

File, HTTP, stream, WebRTC, and receive endpoints are public by default. Once
DNS and TLS are ready, creation prints the stable
`https://<public-id>.tunnel.b3m.dev` URL. Use `--no-public` for the legacy relay
URL, or `--public-endpoint-key <key>` to choose the durable local identity used
to resume the same public ID. `--standbys`, `--relay-id`, and
`--shutdown-grace` control relay redundancy, primary pinning, and graceful
drain. Native public TCP is not supported by the hostname-based ingress.

File sources and receive destinations can opt into an authenticated,
S3-compatible interface:

```bash
beam tunnel expose file ./example.bin --object-storage
beam tunnel receive ./incoming --object-storage \
  --object-storage-config ./.data/incoming-object-storage.json
```

Without `--object-storage-config`, `beam` prints the provider JSON to stdout.
With the flag, it writes the same JSON with mode `0600` and prints its absolute
path. The output can be passed directly to Beam SDK
`S3CompatibleProviderConfig`; it includes the stable endpoint, bucket, key,
region, path-style setting, access key, and secret.

## Architecture and ownership

| Component | Owns | Does not live in `beam` |
| --- | --- | --- |
| `beam` | OAuth Device Grant session, argument parsing, validation, local action execution, Room and Tunnel presentation, config, output, daemon lifecycle, local API clients | auth server, Beam API, Registry server, room state, transfer state, Tunnel networking |
| Beam Auth | central user authentication and token issuance | CLI credential storage and presentation |
| Beam API | RS256 bearer validation, profile, and organization context | OAuth issuance and local credential storage |
| `beam-website/apps/api` | Registry HTTP contract, manifests, publishing policy, package data | login, logout, CLI identity presentation |
| `beam-tunnel-agent` | local API server, identity, network protocols, endpoints, long operations, resume, receipts, worker supervision | public command parsing and presentation |

Operator binaries such as `beam-coordinator`, `beam-relay`, `beam-validator`,
and `beam-dashboard` remain separate. See
[`docs/architecture.md`](docs/architecture.md),
[`docs/authentication.md`](docs/authentication.md), and
[`docs/daemon-local-api.md`](docs/daemon-local-api.md), and
[`docs/rooms.md`](docs/rooms.md). Direct invocation of
`beam-tunnel-agent` is not a supported colleague workflow.

## Configuration

Each release channel owns its own state, so production, development and PR
bundles coexist without sharing configuration, credentials or daemon state. On
macOS and Linux, production uses:

```text
~/.config/beam/config.json
~/.config/beam/credentials.json
~/.beam/agent.sock
```

Development uses `~/.config/beam-dev` and `~/.beam-dev`, and PR bundle *n* uses
`~/.config/beam-pr-<n>` and `~/.beam-pr-<n>`. `XDG_CONFIG_HOME` is honored.
Windows uses the standard user configuration directory and the named pipes
`\\.\pipe\beam-agent`, `\\.\pipe\beam-agent-dev` and
`\\.\pipe\beam-agent-pr<n>`.

Machines configured before channel isolation keep development state in the
production namespace; see
[`docs/installing.md`](docs/installing.md) for the one-time cleanup.

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
| `BEAM_TUNNEL_AGENT_BINARY` | companion tunnel-agent binary override used by `beam tunnel start` |
| `BEAM_AGENTD_BINARY` | deprecated compatibility alias for `BEAM_TUNNEL_AGENT_BINARY` |
| `BEAM_AGENT_LOG` | daemon log path override |
| `BEAM_CONFIG_DIR` | configuration and credentials directory |
| `BEAM_CDN_BASE_URL` | release CDN base URL used by the installers and `beam update`; defaults to the bundle's own channel |
| `BEAM_OUTPUT` | `human`, `json`, or `quiet` |
| `BEAM_SKIP_ONBOARDING` | skip the first-install interactive onboarding when set to `1`, `true`, or `yes` |
| `BEAM_POWERSHELL_PROFILE` | PowerShell profile path supplied by the Windows installer for completion setup |

Production bundles inject the production service endpoints, while `make build`
on `dev` injects `https://auth.b1m.ai`, `https://api.b1m.ai`,
`https://coordinator.b1m.ai`, and `https://cdn.b1m.ai/cli`. Onboarding
therefore remains `beam setup` in production and `beam-dev setup` in
development. Use
`BEAM_COORDINATOR_URL` or `--coordinator` only to select a private control plane
explicitly.

## Output

Human-readable output is the default. Use global `--json` anywhere in a
command for automation:

```console
beam registry versions @beam/example --json
beam room list --json
beam --json tunnel endpoints
```

Successful non-streaming commands write one JSON value to stdout and no
progress text. Errors are structured JSON on stderr. `beam tunnel logs
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

Development values are injected by the surrounding environment, for example:

```console
rp dev beam auth login
rp dev beam registry publish . --tag latest
rp dev beam tunnel status
```

The repository includes three ignored local profiles consumed by the `rp`
shell helper:

| File | Purpose |
| --- | --- |
| `.env.local` | Registry, Auth, and agent running locally |
| `.env.dev` | shared development Registry and Auth services |
| `.env.prod` | production technical endpoints |
| `.env.example` | versioned, secret-free template |

With an `rp` helper configured to resolve the profiles next to the installed
`beam` binary, commands can run from any directory:

```console
rp local beam auth login
rp dev beam registry versions @beam/example
rp prod beam auth whoami
```

The three active profiles are ignored by Git. They must contain technical
overrides only—never tokens, passwords, API keys, or other secrets. The CLI
itself still has no `dev`, `local`, or `prod` profile flags.

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
