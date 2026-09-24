# Install, update, and uninstall

Compiled bundles are served from a per-channel CDN; each archive contains a
matching CLI and tunnel-agent pair and is verified against the published
SHA-256 checksum.

| Channel | CLI command | Agent binary | Distribution |
| --- | --- | --- | --- |
| Production | `beam` | `beam-tunnel-agent` | `https://cdn.b1m.ai/cli` |
| Development | `beam-dev` | `beam-tunnel-agent-dev` | `https://cdn.b1m.ai/cli` |
| Agent PR 27 | `beam-pr-27` | `beam-tunnel-agent-pr27` | `https://cdn.b1m.ai/cli` |

Each channel serves its own installer, and an installer downloaded from a
channel defaults to that channel. macOS/Linux installation:

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
manifest is promoted only after a coordinated build from the current `beam-cli`
and `beam-tunnel-agent` commits of the matching branch — `prod` for production,
`dev` for development — has uploaded all platforms. The installer verifies
SHA-256, stages both executables from the same archive, and then replaces them
together. When upgrading from the previous naming scheme, it stops and removes
`beam-cli` or `beam-cli-dev` after installing `beam` or `beam-dev`. Set
`BEAM_VERSION=v1.2.3` to pin a version, `BEAM_INSTALL_DIR` to change the
destination, or `BEAM_CDN_BASE_URL` for a technical mirror override.

### Channel isolation

Every channel keeps its machine-local state under its own namespace, so
production, development and PR bundles can be installed and configured on one
machine without disturbing each other:

| Channel | Configuration and credentials | Socket, logs, daemon state | Room data port |
| --- | --- | --- | --- |
| Production | `~/.config/beam` | `~/.beam` | 8621 |
| Development | `~/.config/beam-dev` | `~/.beam-dev` | 8622 |
| PR 27 | `~/.config/beam-pr-27` | `~/.beam-pr-27` | derived from the PR number |

Windows uses the corresponding user configuration directory and the named pipes
`\\.\pipe\beam-agent`, `\\.\pipe\beam-agent-dev` and
`\\.\pipe\beam-agent-pr27`.

This matters because `config.json` outranks a bundle's compiled-in endpoints. A
shared namespace would let whichever channel ran `setup` last decide the
endpoints for both, so a production CLI could operate against the development
environment with no indication.

#### Machines configured before channel isolation

Installations configured before this separation kept development state in
`~/.config/beam` and `~/.beam`, which are now the **production** namespace. A
production bundle on such a machine still reads that development configuration,
and `beam setup` can refuse to enrol because the daemon state already records a
Room registration. Check with:

```console
beam config
```

If the endpoints name `*.b1m.ai`, the state predates this change. Move it to
the development namespace:

```console
beam-dev tunnel stop
mv ~/.config/beam ~/.config/beam-dev
mv ~/.beam ~/.beam-dev
beam-dev config unset agent_socket
```

The last step matters: `setup` records `agent_socket` as an absolute path, so a
moved configuration would keep pointing at `~/.beam/agent.sock`, which is now
the *production* socket. Unsetting it makes the value derive from the channel
namespace again. Confirm both channels afterwards:

```console
beam config          # endpoints must name *.b1m.ai
beam-dev config      # endpoints must name *.b1m.ai
```

`beam` then starts from its own compiled-in production defaults, and `beam-dev`
keeps the configuration it already had. To discard the old state instead of
keeping it, remove those two directories rather than moving them, and run
`beam setup` again.

Each bundle includes the service URLs for its channel. The complete machine
onboarding command for a development installation is therefore:

```console
beam-dev setup
```

Development builds default to `https://auth.b1m.ai`,
`https://api.b1m.ai`, `https://coordinator.b1m.ai`, and
`https://cdn.b1m.ai/cli`; production builds default to the corresponding
`https://*.b1m.ai` services and `https://cdn.b1m.ai/cli`. The
`BEAM_REGISTRY_URL`, `BEAM_AUTH_URL`, `BEAM_API_URL`, and
`BEAM_COORDINATOR_URL` variables remain available as explicit
private-environment overrides.
Set `BEAM_CONSOLE_URL` as well when the private environment has its own web
onboarding page.

### Install an isolated pull-request bundle

After the agent PR checks and coordinated bundle workflow pass, install its
latest commit on macOS or Linux without replacing production, dev, or another
PR bundle:

```console
curl -fsSL https://cdn.b1m.ai/cli/install.sh | BEAM_PR=27 sh
beam-pr-27 setup
```

On Windows PowerShell:

```powershell
$env:BEAM_PR = "27"
irm https://cdn.b1m.ai/cli/install.ps1 | iex
beam-pr-27 setup
```

PR bundles use the same services as `beam-dev`, including
`https://coordinator.b1m.ai`, but keep their machine-local resources under
their own namespace, as described in [Channel isolation](#channel-isolation).
The Room adapter uses a stable PR-specific loopback port.

`beam-pr-27 update` follows `pr-27.json`, so it only advances that candidate.
To uninstall it, stop `beam-pr-27`, remove `beam-pr-27` and
`beam-tunnel-agent-pr27`, then optionally remove its two namespaced directories.

Machines that only need to join an existing Room select that path in
`beam-dev setup`. They can also bootstrap from the owner's invitation
directly:

```console
beam-dev room join ROOM_ID --invitation-file invitation.token
```

Windows PowerShell:

```powershell
irm https://cdn.b1m.ai/cli/install.ps1 | iex
```

Use `https://cdn.b1m.ai/cli/install.ps1` for a development installation.

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
beam-dev update
```

`beam-dev update --check` checks the latest manifest without downloading or
stopping the daemon. `beam-dev update --version VERSION` selects a published bundle,
and `beam-dev update --force` reinstalls the selected version. The command verifies
the archive checksum before it asks `beam-tunnel-agent-dev` to stop. On Windows, it
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
Without it, each bundle updates from the channel it was built for, so a
development bundle never resolves a production release or the reverse.

### Bundles installed before the `cdn.b1m.ai` move

Bundles published before this change resolve `https://cdn.b3m.dev/cli`, which no
longer receives publications. `beam-dev update` on such an installation reports
no newer version indefinitely. Reinstall once from the development channel to
move onto `cdn.b1m.ai`:

```console
curl -fsSL https://cdn.b1m.ai/cli/install.sh | sh
```

Later updates then follow the new channel automatically.

To uninstall development builds, run `beam-dev tunnel stop`, then remove
`beam-dev` and `beam-tunnel-agent-dev` (with `.exe` on Windows) from the
install directory. For production, use the same steps without the `-dev`
suffix. Then optionally remove that channel's own configuration and daemon
state — `~/.config/beam-dev` and `~/.beam-dev` for development,
`~/.config/beam` and `~/.beam` for production. Removing state also removes resumable operation records
and stable endpoint identity, so it is intentionally not automated.
