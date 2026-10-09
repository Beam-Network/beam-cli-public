# Beam CLI command reference

The public CLI is organized around common tasks. Commands that need the
tunnel agent start it automatically; `beam agent status` only reports
and shows a stopped agent as stopped. Existing command spellings remain
supported for scripts.

Only Rooms are available for now. The tunnel commands (`share`, `receive`,
`unshare`, `transfer`, `operation`, and `tunnel`, except the `tunnel room` and
`tunnel studio` aliases) are hidden from help and completion and exit with code
2. Set `BEAM_EXPERIMENTAL_TUNNELS=1` to re-enable them for internal testing.

Global options can be placed anywhere:

```text
--json                  Machine-readable output
--quiet, -q             Minimal successful output
--context, -c <name>    Select a named context
--no-interactive        Disable prompts and browser interaction
--verbose, -v           Include underlying error causes
```

## Terminal output

Beam styles its output only when it is writing to an interactive terminal.
Everything below degrades to the plain text that earlier versions produced, so
scripts, pipes, and redirects are unaffected.

Styling is suppressed when:

- standard output is piped or redirected, for listings and help;
- standard error is piped or redirected, for errors and hints;
- `NO_COLOR` is set, or `TERM` is `dumb`;
- `--json` or `--quiet` is in effect.

`BEAM_ASCII_BOX=1` replaces box drawing with ASCII characters. It is applied
automatically on the legacy Windows console.

### Help

`beam --help` and `beam help <command>` group related commands into titled
panels on a terminal, sized to the terminal width and capped so descriptions
stay beside their names. A terminal narrower than 40 columns, or any
non-terminal destination, gets the plain indented form instead. `--json`
always embeds the plain text.

### Errors

`Error:` and `Hint:` labels are colored; the message itself is not, since the
message is the part that has to be read word by word.

### Listings

Commands that list resources print a header row and columns padded to a common
width, so identifiers of different lengths stay aligned:

```console
$ beam room list
ROOM ID              STATE   ORGANIZATION  MEMBER
room_a               active  org_a         mem_a
room_bbbbbbbbbbbbbb  closed  org_a         mem_bbbbbbbb
```

Columns are separated by two spaces and values are never truncated, so an
identifier can always be copied whole. An empty listing prints nothing, not a
lone header.

`--quiet` prints bare identifiers with no header.

Streaming commands — `watch` and `logs` — emit each row as its event arrives
and are not aligned, because the rows that would set the column widths have not
happened yet.

## Start and diagnose

```text
beam setup                         Login, select an organization, connect the agent, and start the daemon
beam status                        Show account, context, agent, and daemon status
beam doctor [--fix]                Diagnose Beam and apply safe local repairs
beam logs [--follow]               Show daemon logs
beam version | beam --version      Show the CLI build
beam update [--check]              Check or install a matched CLI/daemon bundle
```

Interactive `setup` asks whether to connect a Beam account, join a Room from an
invitation, or defer configuration. Account onboarding asks for a machine name
and prompts for an organization only when no unambiguous default exists. The
wizard then offers completion for the detected Bash, Zsh, Fish, or PowerShell
environment.

`doctor` accepts a connected, registered invitation-only room identity without
an account login. Account-mode machines still need their active login; malformed
credentials and disconnected or unregistered agents remain diagnostic failures.

Setup is resumable. A valid session, an existing agent registration, and the
saved organization are reused instead of recreated. If the account has no
organization, setup keeps the login and offers to refresh after an organization
is created, join a Room instead, or finish later. Restricted organizations are
shown as unavailable. A saved organization that was deleted or removed from the
account is discarded in favor of the currently accessible list. Setup opens the
Beam Console onboarding page that matches the build's compiled-in API;
`BEAM_CONSOLE_URL` overrides that page for private environments.

For automation:

```text
beam setup --no-interactive --mode account [--organization ORG] [--label NAME]
beam setup --no-interactive --mode room --room ROOM_ID (--invitation-file FILE | --invitation-token TOKEN)
beam setup --no-interactive --mode skip
```

Non-interactive setup always requires `--mode`; it never guesses a workflow or
waits for input. JSON errors retain their numeric `code` and can also expose a
stable `kind`. Onboarding callers should handle at least:

| Kind | Meaning |
| --- | --- |
| `setup_mode_required` | `--mode` was omitted in a non-interactive invocation |
| `organization_required` | the account has no organization |
| `organization_selection_required` | multiple organizations exist without a default |
| `organization_not_found` | an explicitly selected organization is inaccessible |
| `organization_unavailable` | all matching organizations are restricted |
| `account_unavailable` | the Beam account itself is restricted |
| `agent_configuration_conflict` | the registered machine belongs to another organization, coordinator, or Room-only identity |
| `agent_revoked` | the local agent identity was revoked |
| `invitation_invalid`, `invitation_expired`, `invitation_revoked`, `invitation_consumed`, `invitation_room_mismatch` | Room invitation failures |

An already registered machine is accepted only when the requested coordinator
and organization are compatible. Setup never silently moves an agent. Use
`beam agent disconnect` before intentionally changing ownership or control
planes.

## Account, organization, and session

```text
beam login                         Sign in with the OAuth device flow
beam logout                        Revoke and remove the local session
beam whoami                        Refresh and show the Beam identity
beam session status|refresh        Inspect or refresh the session
beam org list|current              List organizations or show the selected one
beam org use|show <id-or-slug>     Select or inspect an organization
```

`beam auth login|logout|whoami` remains available as a compatibility spelling.

## Configuration and contexts

```text
beam config path|list|validate
beam config get <key>
beam config set <key> <value>
beam config unset <key>
beam config edit

beam context list|current
beam context create|use|show|delete <name>
```

Named context files live under the Beam configuration directory.

## Rooms

Resource-scoped syntax:

```text
beam room create [--api-key <key>]|list [--state <active|closed|all>]
beam room join <room> --invitation-file <file> [--coordinator <url>]
beam room <room> show|refresh|watch|leave|close
beam room <room> invite [--role role-id] [--channel-access channel-id=action,...] [options] [--out invitation.token]
beam room <room> invitation list|show|revoke [invitation-id]
beam room <room> member list|show|watch|remove [member-id]
beam room <room> role list|show|create|assign|revoke|delete [role-id]

beam room <room> channel list|create
beam room <room> channel <channel> show|update|close|publish|listen
beam room <room> channel <channel> stream publish|listen
beam room <room> channel <channel> datagram send|listen
beam room <room> channel <channel> object publish (--file <path> | --from <member-id> --object-key <key>) [--to <member-id>] [--allow-partial]|list|show|watch|cancel
beam room <room> channel <channel> media publish [--as <name>] [--snippets]
beam room <room> channel <channel> media view [--session <workload-id> | --all] [--sdp] [--open] [--snippets]
beam room <room> channel <channel> media list|stop [workload-id]
beam room <room> channel <channel> persistence status|retry|acknowledge|purge
beam room <room> channel <channel> grant list|put|revoke
```

`media publish --as` names the publisher (default `main`); each name has its own
WHIP URL and token, and a token for one name is refused on another.
`media view --session` watches one publisher from `media list` (default: the
most recent), `--all` watches every publisher, and `--sdp` describes each stream
as loopback RTP in an SDP file.

The previous action-first syntax remains an exact compatibility path.

## Actions and Registry

```text
beam action init [directory] [--name @scope/name]
beam action inspect|validate [directory]
beam action run [directory] [--watch]
beam action pack [directory] [--out file]
beam action publish [directory] [--tag tag]
beam action version [directory]

beam registry search [query]
beam registry show <package>
beam registry versions <package>
beam registry resolve <package> [--range range]
beam registry download <package>@<version> [--out file]
beam registry verify <archive>
```

`registry inspect|pack|publish` remain compatibility aliases for the local
Action workflow.

`pack` and `publish` archive every regular file in the Action directory, or
only the files matched by `files` in its `package.json` (npm semantics). A
`.beamignore` file (gitignore syntax) excludes more. A built-in deny-list
always wins: the directories `.git/`, `node_modules/`, `.beam-packages/`,
`fixtures/`, `test/`, `tests/` and `__tests__/` at any depth, and the files
`.env`, `.env.*`, `.npmrc`, `*.local`, `*.pem`, `*.test.*`, `*.spec.*`,
`.DS_Store`, `.beamignore`, `*.map`, and SSH keys under their default names
(`id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`, each also with a `.pub`,
`-cert` or `-cert.pub` suffix). `beam-action.json`, `beam-project.json`,
`package.json` and the entrypoint are always archived; an entrypoint on the
deny-list (for example `dist/main.test.mjs`) fails the pack.

`publish` uploads the archive as `multipart/form-data` (`metadata` JSON and a
binary `artifact` part) and retries once with the legacy JSON body when the
Registry cannot read multipart. The Registry derives trust level, validation
status and publisher from the session. `registry download` verifies the bytes
against the version's `artifactChecksum` (and the `Digest` header, which must
agree with it) before writing, prefers the version's signed `artifactUrl`, and
sends the Beam session token only to the Registry origin.

## Agent, Studio, and completion

```text
beam agent connect|start|stop|restart|status|diagnostics
beam agent rename <label>
beam agent logs [--follow]
beam agent repair
beam agent credentials recover
beam agent disconnect
beam agent revoke --yes

beam studio connect <url> [options]
beam studio status|permissions|disconnect
beam studio update-permissions [options]

beam completion install [bash|zsh|fish|powershell]
beam completion generate <bash|zsh|fish|powershell>
beam completion uninstall
```

PowerShell completion is installed as a marked, idempotent block in the current
user's `$PROFILE`. Uninstalling completion removes only that Beam-managed block
and preserves the rest of the profile.

`agent disconnect` removes the local enrollment and stops the daemon so no
runtime keeps using the former credential; it does not revoke the remote agent
record. `studio update-permissions` changes only the explicitly supplied local
policy fields and reconnects the Studio session when necessary.
