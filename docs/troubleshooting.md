# Troubleshooting

- Exit 6 means the daemon could not be started automatically. Verify that the
  companion binary is installed and inspect `~/.beam/agent.log` (or
  `BEAM_AGENT_LOG`).
- Exit 10 with kind `agent_request_interrupted` means a request connected to
  the daemon, but the connection broke or timed out before it replied. The
  daemon may have carried the request out, so Beam does not send it again:
  check whether it took effect (for example `beam agent status`, or the
  matching `list` or `show` command) before running the command again.
- Exit 7 means no local API overlap or mismatched tagged bundle versions.
  Reinstall both binaries from one archive; never update only one.
- Use `beam agent status --json` and `beam agent diagnostics --json` for
  identity, connection, and runtime health.
- `Room control: degraded` in `beam agent status` (exit 10 with kind
  `room_control_degraded` from `beam room` commands) means the agent runs but
  cannot reach the coordinator for Rooms: its room discovery failed, which
  blocks every Room command, or one Room's lease resume or renewal has kept
  failing for 10 seconds, which blocks only that Room's commands. Check
  network access to the coordinator and `beam agent logs`; Room control
  recovers on its own within seconds of the coordinator answering.
- Use `beam logs --follow` for runtime diagnostics.
- A Unix socket must be owned by the user and mode `0600`. A non-socket at the
  configured path is never overwritten.
- On Windows, ensure the named pipe belongs to the same user session and stop
  the daemon before replacing executables.
- `BEAM_TUNNEL_AGENT_BINARY` is the supported binary override.
  `BEAM_AGENTD_BINARY` remains a deprecated compatibility alias. The old
  `BEAM_AGENT_BINARY` is migration-only.

Never place coordinator, relay, tunnel, or bridge secrets directly in a
process command line. Pass paths to owner-only session/lease files through
`beam`; the daemon reads the files inside its worker boundary.
