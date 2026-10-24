# `beam` / tunnel agent local API

Status: **protocol v13**, compatible down to **v4**. Room operations require
at least **v8**. The authoritative implementation lives with the tunnel agent;
`internal/tunnel` in this repository contains DTOs and an HTTP client only.

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
capabilities, readiness, and the configured agent identity. An agent that runs
Room control adds `room_control`, whether it reaches the coordinator:

```json
"room_control": {"state": "degraded", "since": "2026-10-10T12:00:11Z",
 "checked_at": "2026-10-10T12:00:31Z", "operation": "renewal", "room_id": "btr_room_a", "code": "unavailable",
 "failing_rooms": [{"room_id": "btr_room_a", "operation": "renewal", "code": "unavailable",
   "since": "2026-10-10T12:00:01Z"}]}
```

`state` is `connecting` until the agent's first room discovery finishes;
`degraded` while the latest discovery failed, or while a Room's lease resume or
renewal has kept failing for 10 seconds; `healthy` otherwise. `since` is when
it began. While degraded, `operation` (`discovery`, `resume` or `renewal`),
`room_id` (for a resume or renewal) and `code` (the coordinator's error code,
or `unavailable` when no coordinator answered) name the cause: the failed
discovery while it fails, otherwise the Room failing longest. `failing_rooms`
lists every Room whose lease keeps failing, each with its own operation, code
and `since`. `beam room` uses them to block every command on a failed
discovery, and only the commands naming a failing Room otherwise. The field is
absent when the agent runs no Room control.

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
| `GET` | `/v1/rooms/{room_id}/inbox` | list the objects delivered to this agent in the Room |
| `GET` | `/v1/rooms/{room_id}/inbox/{transfer_id}` | read one delivered object; honours `Range` |
| `DELETE` | `/v1/rooms/{room_id}/inbox/{transfer_id}` | remove one delivered object from this agent's inbox |
| various | `/v1/rooms/{room_id}/roles`, `/channels`, `/grants` | manage authorization and channel policy |
| `GET`, `POST` | `/v1/rooms/{room_id}/webhooks` | list a Room's webhook members, or add one |
| `GET`, `DELETE` | `/v1/rooms/{room_id}/webhooks/{member_id}` | inspect or remove a webhook member |
| `POST` | `/v1/rooms/{room_id}/webhooks/{member_id}/resume` | resume a paused or degraded webhook |
| `POST` | `/v1/rooms/{room_id}/webhooks/{member_id}/host` | move a webhook to another host member |
| `GET` | `/v1/hosted-webhooks` | list the webhooks this agent hosts or is asked to host |
| `POST` | `/v1/hosted-webhooks/{room_id}/{member_id}/accept` | accept with the webhook URL and secret |
| `POST` | `/v1/hosted-webhooks/{room_id}/{member_id}/decline` | decline a proposed webhook |
| `POST` | `/v1/hosted-webhooks/{room_id}/{member_id}/secret` | rotate the signing secret |
| `POST` | `/v1/hosted-webhooks/{room_id}/{member_id}/test` | send a signed test event |

`beam room <room> webhook` uses the `/v1/rooms/{room_id}/webhooks` routes when
`/v1/status` advertises the capability `btr.webhook.manage`, and the
`/v1/hosted-webhooks` routes when it advertises `btr.webhook.host`; without
the capability the CLI fails before sending anything. Lists return
`{"webhooks": [...]}` and single webhooks `{"webhook": {...}}`, a view that
never carries the webhook URL or secret; its `presence` is the webhook
member's own presence and its `host_presence` the host member's (`online` or
`offline`). The CLI says a webhook delivers only while its state is `active` or
`degraded` and both are `online`. A view read from the coordinator
carries `upgrade_required_member_ids` when members that manage the webhook's
channels run an agent without webhook support; the CLI prints them as a
`Blocked` line. `add`, `remove`, `resume`, and `move`
send an `Idempotency-Key`. The accept and secret request bodies are the only
place the URL and secret travel; the CLI never prints them.

The inbox routes need `btr.inbox.list`, `btr.inbox.read` and
`btr.inbox.delete` in `/v1/status`. A list returns `{"objects": [...]}`, each
with `room_id`, `transfer_id`, `member_id`, `filename`, `size_bytes`,
`completed_at` and `expires_at`. A read streams the object's bytes; with
`Range: bytes=<start>-<end>` it answers `206 Partial Content` with only that
range, so a client can read part of a large object without copying it.

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
    "message": "the tunnel agent supports local API 4-13",
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
non-overlapping change must ship `beam` and the tunnel agent in the same release
archive.
