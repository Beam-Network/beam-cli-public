# CLI architecture

## Public boundary

Colleagues use one executable only: `beam`. The release bundle also contains
the tunnel agent, a companion daemon which `beam` discovers next to itself and
then in `PATH`. `BEAM_TUNNEL_AGENT_BINARY` is the explicit technical override;
`BEAM_AGENTD_BINARY` remains a deprecated compatibility alias.

```text
user -> beam -> auth / config / validation / output
                 |
                 +-- OAuth ------------> Beam Auth
                 +-- RS256 bearer -----> Beam API / Registry
                 |
                 +-- private local API -> tunnel agent -> coordinator / relay
                         room + tunnel
```

`beam` owns command parsing, help, completion, authentication, configuration,
human/JSON/quiet output, exit codes, daemon lifecycle, and presentation of
structured daemon errors. It does not import Tunnel, QUIC, WebRTC, yamux,
relay, coordinator, or transfer-engine packages.

The tunnel agent owns all network protocols, endpoint runtimes, distributed
upload/download, private bridges, durable operation state, resumption,
receipts, logs, metrics, and worker supervision. Its process arguments are
limited to internal startup configuration. Network workers re-enter the same
binary through the undocumented `__worker` boundary and are never a public
command surface.

## Local packages

- `cmd/beam`: executable entrypoint.
- `internal/command`: user command validation and orchestration.
- `internal/config`: user configuration and technical overrides.
- `internal/auth`: OAuth Device Grant, refresh rotation, and secure credential storage.
- `internal/beamapi`: bearer injection, one-refresh/one-retry policy, and account context.
- `internal/registry`: Registry client and deterministic action packaging.
- `internal/roomcli`: Room command parsing, validation, and rendering over the
  versioned local API. It contains no Room runtime or durable state.
- `internal/tunnel`: DTO-only local API client and daemon process lifecycle.
- `internal/output`: human, JSON, quiet, and progress rendering.
- `internal/version`: build-injected bundle metadata.

The DTO mirrors in `internal/tunnel` and `internal/roomcli/protocol` are
intentionally network-engine-free. Their contract tests run against a local
socket fake; the authoritative server DTOs and reciprocal tests live with the
tunnel agent.

`beam room` is the canonical Room namespace. `beam tunnel room` enters the
same in-process command runner for compatibility. Neither path resolves or
executes a `beam-room` companion:

```text
beam room ... ────────────┐
                         ├─> owner-only socket ─> tunnel agent ─> coordinator
beam tunnel room ... ─────┘   compatibility alias
```

## Security and lifecycle

- Unix sockets are mode `0600` inside an owner-only directory.
- Windows named pipes grant access to their current-user owner only.
- Every request negotiates a supported local API range and carries the CLI
  bundle version. Tagged builds reject a daemon from another bundle.
- Credentials and bearer tokens are never placed in daemon process arguments.
- Access tokens remain in memory; rotating refresh tokens use the OS credential
  store or an atomically replaced owner-only fallback file.
- Long operations are stored by the daemon and outlive the invoking CLI.
- `beam tunnel stop` asks the daemon to cancel supervised runtimes and shut
  down its local listener cleanly.
