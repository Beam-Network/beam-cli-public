# `beam` / `beam-tunnel-agent` local API

Status: **protocol v13**, compatible down to **v4**. Room operations require
at least **v8**. The authoritative implementation is in
`beam-tunnel/services/agent/internal/localdaemon`; `beam-cli/internal/tunnel`
contains DTOs and an HTTP client only.

## Transport and negotiation

- macOS/Linux: HTTP/1.1 over `~/.beam/agent.sock`, socket mode `0600`.
- Windows: HTTP/1.1 over `\\.\pipe\beam-agent`, restricted to the owner.
- Technical socket override: `BEAM_AGENT_SOCKET`.
- JSON: `application/json`; followed logs: NDJSON.

Every request sends:

```text
Beam-Local-API-Min: 4
Beam-Local-API-Max: 13
Beam-CLI-Version: <bundle version>
```

The response selects a version in `Beam-Local-API-Version`. With no overlap,
or when tagged CLI and daemon bundle versions differ, the daemon returns HTTP
426 and a structured error before executing the operation. The legacy
`Beam-CLI-Protocol` header is sent during the migration window but does not
replace range negotiation.

`GET /v1/status` returns build provenance, supported protocol range,
capabilities, readiness, and the configured agent identity.

`beam room` narrows its request range to `8-13`, so an older daemon fails with
a protocol error before any Room mutation. `beam tunnel room` uses the same
client and range because it is only a command alias.

## Resources

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/status` | readiness, build and protocol negotiation |
| `GET` | `/v1/agent-connection` | canonical agent identity and heartbeat state |
| `POST` | `/v1/agent-connection/prepare` | create/load the durable machine key |
| `POST` | `/v1/agent-connection` | redeem a one-time coordinator enrollment |
| `POST` | `/v1/agent-connection/room-invitation` | enroll the machine and join a Room without a Beam user account |
| `POST` | `/v1/agent-connection/recover` | rotate the runtime bearer by signed challenge |
| `POST` | `/v1/agent-connection/revoked` | remove the local bearer after coordinator revocation |
| `POST` | `/v1/exposures` | create file/HTTP/stream/WebRTC/TCP endpoint |
| `POST` | `/v1/destinations` | create a directory destination |
| `GET` | `/v1/endpoints` | list endpoints |
| `GET` | `/v1/endpoints/{id}` | inspect an endpoint |
| `POST` | `/v1/endpoints/{id}/close` | close an endpoint |
| `POST` | `/v1/operations` | start upload/download/bridge/duplex/identity work |
| `GET` | `/v1/operations` | list durable operations |
| `GET` | `/v1/operations/{id}` | inspect status, result, or error |
| `POST` | `/v1/operations/{id}/cancel` | cancel supervised work |
| `GET` | `/v1/metrics` | local endpoint and operation counters |
| `GET` | `/v1/logs?follow=<bool>` | snapshot or follow local logs |
| `POST` | `/v1/shutdown` | gracefully stop the daemon |
| `GET`, `POST` | `/v1/rooms` | list or create Rooms |
| `GET` | `/v1/rooms/{room_id}` | inspect the local Room snapshot |
| `POST` | `/v1/rooms/{room_id}/join` | join with a single-use invitation |
| `POST` | `/v1/rooms/{room_id}/leave` | leave a Room |
| `POST` | `/v1/rooms/{room_id}/close` | close an owned Room |
| `POST` | `/v1/rooms/{room_id}/refresh` | refresh coordinator metadata |
| `POST` | `/v1/rooms/{room_id}/invitations` | create a bounded invitation |
| `GET` | `/v1/rooms/{room_id}/memberships` | list Room members |
| various | `/v1/rooms/{room_id}/roles`, `/channels`, `/grants` | manage authorization and channel policy |

An accepted operation receives an `op_...` ID and moves through `queued`,
`running`, and one of `succeeded`, `failed`, or `cancelled`. The complete
request and state are written to an owner-only durable journal. Active records
found after an unclean daemon restart are replayed; transfer workers then use
their range/multipart journals to resume useful work.

## Errors

All non-2xx responses use one envelope:

```json
{
  "error": {
    "code": "protocol_incompatible",
    "message": "beam-tunnel-agent supports local API 4-13",
    "retryable": false,
    "details": {"daemon_min": 4, "daemon_max": 13}
  }
}
```

Stable codes include `protocol_incompatible`,
`bundle_version_incompatible`, `endpoint_not_found`,
`operation_not_found`, `invalid_operation`, and `operation_failed`.

## Compatibility policy

Protocol v4 is the compatibility floor for durable operations and explicit
version-range negotiation. Room control entered the contract at v8, and the
current daemon advertises v10. v3 clients and daemons remain discoverable only
so `beam` can report a clear incompatibility; no mutating request is
downgraded. A future version may overlap by widening `Min`/`Max`, but a
non-overlapping change must ship `beam` and `beam-tunnel-agent` in the same release
archive.
