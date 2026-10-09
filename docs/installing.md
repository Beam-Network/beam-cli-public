# Install, update, and uninstall

Compiled bundles are served from the release CDN; each archive contains a
matching CLI and tunnel-agent pair and is verified against the published
SHA-256 checksum. The production channel installs `beam` from
`https://cdn.b1m.ai/cli`.

An installer downloaded from a channel defaults to that channel. macOS/Linux
installation:

```console
curl -fsSL https://cdn.b1m.ai/cli/install.sh | sh
```

On a first installation from an interactive terminal, the installer immediately
launches the shared `beam setup` onboarding. It offers account enrollment,
Room invitation enrollment, or deferring configuration. Set
`BEAM_SKIP_ONBOARDING=1` to keep CI and other unattended installs non-interactive.
The wizard records completed authentication and configuration checkpoints, so
rerunning `beam setup` resumes rather than creating duplicate machine state.

The installer resolves `latest.json` under its own channel base URL. That
manifest is promoted only after a coordinated build of the CLI and the tunnel
agent has uploaded all platforms. The installer verifies SHA-256, stages both
executables from the same archive, and then replaces them together. When
upgrading from the previous naming scheme, it stops and removes the old CLI
binary after installing `beam`. Set `BEAM_VERSION=v1.2.3` to pin a version,
`BEAM_INSTALL_DIR` to change the destination, or `BEAM_CDN_BASE_URL` for a
technical mirror override.

### Channel isolation

Every release channel keeps its machine-local state under its own namespace,
so bundles from different channels can be installed and configured on one
machine without disturbing each other. Production uses:

| Configuration and credentials | Socket, logs, daemon state | Room data port |
| --- | --- | --- |
| `~/.config/beam` | `~/.beam` | 8621 |

Windows uses the corresponding user configuration directory and the named pipe
`\\.\pipe\beam-agent`. Another channel uses the same layout under its own
`beam-<channel>` name.

This matters because `config.json` outranks a bundle's compiled-in endpoints. A
shared namespace would let whichever channel ran `setup` last decide the
endpoints for both, so a CLI could operate against the wrong environment with
no indication.

Each bundle includes the service URLs for its channel. Production builds
default to `https://auth.b1m.ai`, `https://api.b1m.ai`,
`https://coordinator.b1m.ai`, and `https://cdn.b1m.ai/cli`. The
`BEAM_REGISTRY_URL`, `BEAM_AUTH_URL`, `BEAM_API_URL`, and
`BEAM_COORDINATOR_URL` variables remain available as explicit
private-environment overrides.
Set `BEAM_CONSOLE_URL` as well when the private environment has its own web
onboarding page.

Machines that only need to join an existing Room select that path in
`beam setup`. They can also bootstrap from the owner's invitation directly:

```console
beam room join ROOM_ID --invitation-file invitation.token
```

Windows PowerShell:

```powershell
irm https://cdn.b1m.ai/cli/install.ps1 | iex
```

The installer adds the install directory (by default
`%LOCALAPPDATA%\Beam\bin`) to both the current PowerShell process and the
current user's persistent `PATH`. The command is therefore available
immediately, including after opening a new terminal.

The Windows installer also launches `beam setup` on a first interactive
installation and passes the current PowerShell profile so optional completion
can be installed without guessing its location.

The wizard can also be rerun at any time:

```console
beam setup
```

Non-interactive examples:

```console
beam setup --no-interactive --mode account --organization ORG --label MACHINE
beam setup --no-interactive --mode room --room ROOM_ID --invitation-file invitation.token
```

`--mode` is mandatory whenever setup is non-interactive, including JSON output.
If account enrollment cannot continue because the account has no accessible
organization, the OAuth session remains stored and the command can be retried
after the organization is created or restored.

## Update

Once installed, update both binaries with the public CLI:

```console
beam update
```

`beam update --check` checks the latest manifest without downloading or
stopping the daemon. `beam update --version VERSION` selects a published bundle,
and `beam update --force` reinstalls the selected version. The command verifies
the archive checksum before it asks the tunnel agent to stop. On Windows, it
schedules the final replacement after the current CLI process exits.

The updater always targets the running CLI executable and the matching
tunnel-agent binary beside it. It does not accept an arbitrary installation path. If
that directory requires administrator permissions, rerun the platform
installer; the shell installer can request `sudo` for only the final file
replacement:

```console
curl -fsSL https://cdn.b1m.ai/cli/install.sh | sh
```

`BEAM_CDN_BASE_URL` remains a technical override and must identify a trusted
HTTPS mirror. The native updater accepts HTTP only for loopback test servers.
Without it, each bundle updates from the channel it was built for, so a bundle
never resolves another channel's release.

## Uninstall

Run `beam agent stop`, then remove `beam` and the tunnel agent binary installed
beside it, `beam-tunnel-agent`, from the install directory (both with `.exe` on Windows).
Then optionally remove the channel's own configuration and daemon state —
`~/.config/beam` and `~/.beam` for production. Removing state also removes
resumable operation records and stable endpoint identity, so it is
intentionally not automated.
