# Beam Rooms

`beam room` is the public namespace for creating and operating Rooms. Every
command uses the authenticated machine identity already owned by the tunnel
agent:

```text
beam room ... -> owner-only local socket -> tunnel agent -> coordinator
```

There is no `beam-room` process, credential, or state directory.

## Prerequisites

Install a release bundle (`beam` and its tunnel agent), then register the machine:

```console
beam agent connect
beam agent status
```

`status` must report a registered, connected agent. Room commands do not own a
second login or identity and do not start a separate service.

## Create and inspect a Room

Starting a Room is billable, so `create` names the Beam API key it is charged to:

```console
beam room create --api-key "$BEAM_API_KEY"
beam room list [--state <active|closed|all>]
beam room inspect ROOM_ID
beam room refresh ROOM_ID
```

A closed Room is retained by the daemon rather than deleted, so `list` hides
closed Rooms by default and they do not accumulate in the listing. A suspended
Room is still live and stays visible. Pass `--state all` to include closed
Rooms, or a single state to see only those.

When the listing is empty, `list` explains why on stderr and still exits 0:
whether no Rooms exist at all, or Rooms were fetched and hidden by the current
filter. `--json` and `--quiet` are unaffected and print nothing extra, so
scripts read the same bytes as before.

`--api-key` may be omitted when `BEAM_API_KEY` is set; with neither, `create`
fails locally rather than reaching the coordinator. The machine identity decides
what you may do; the API key decides who pays. An organization holds one credit
pool but may issue many keys against it, each with its own cap and monthly
budget, so the key is named rather than inferred.

Use a stable idempotency key when a caller may retry a mutation. The same key
also settles one credit hold rather than two:

```console
beam room create --idempotency-key onboarding-room-42 --json
```

The daemon persists the Room snapshot, authorization epoch, key epoch, and
notification cursor. `refresh` reconciles that local snapshot with the
coordinator without creating a new machine identity.

## Invite and join another agent

The owner creates a bounded invitation:

```console
beam room invite ROOM_ID \
  --principal PRINCIPAL_ID \
  --agent AGENT_ID \
  --role ROLE_ID \
  --channel-access CHANNEL_ID=discover,subscribe \
  --max-uses 1 \
  --ttl 900 \
  --json
```

`--role` and `--channel-access` can be repeated. The coordinator assigns the
selected roles and direct channel grants idempotently as part of each join, so
the owner does not need to configure every new member afterward.

The invitation bearer is returned once. Deliver it through a secure channel.
On the joining machine, prefer an owner-only file so the bearer does not enter
shell history or process arguments:

```console
chmod 600 invitation.token
beam room join ROOM_ID --invitation-file invitation.token
```

Join requests derive a stable, token-safe idempotency key when one is not
provided. Retrying the same invitation therefore does not create duplicate
local enrollment work. Expired, revoked, consumed, invalid, and wrong-Room
invitations have distinct machine-readable error kinds. On Unix, invitation
files must be private; use `chmod 600 invitation.token` when prompted.

No Beam account or `beam agent connect` step is required on the joining
machine. If the local agent has no identity yet, `join` creates its Ed25519
machine key, enrolls it through the invitation, restarts the tunnel agent, and
discovers the joined Room automatically. Source builds without a bundled
coordinator default can add `--coordinator URL`.

This invitation-created identity is join-only: it can use its granted Room
roles and channels, but it cannot create new Rooms. Connecting through a Beam
account is still required for `beam room create`.

The inline `--invitation-token` option is available for controlled automation.
An invitation is consumed by the coordinator and is never written to the
agent's durable journal.

List current membership metadata with:

```console
beam room members ROOM_ID
```

The listing includes both agents and object-storage buckets. Bucket membership
is attached in Studio from an existing organization credential; the CLI shows
its room identity, display name, availability, roles, and grants without ever
receiving the storage credential.

A bucket member's storage credential must not be restricted to specific IP
addresses or networks.

## Roles and grants

```console
beam room role list ROOM_ID
beam room role create ROOM_ID --name "Observer"
beam room role assign ROOM_ID ROLE_ID MEMBER_ID
beam room role revoke ROOM_ID ROLE_ID MEMBER_ID

beam room grant list ROOM_ID CHANNEL_ID
beam room grant put ROOM_ID CHANNEL_ID \
  --subject-type member \
  --subject MEMBER_ID \
  --actions discover,subscribe
beam room grant revoke ROOM_ID CHANNEL_ID GRANT_ID
```

Mutations accept `--epoch`, `--revision`, and `--idempotency-key` where
applicable. If the epoch or revision is omitted, the tunnel agent uses its latest
durable authorization floor.

## Channels

Create and inspect a message channel:

```console
beam room channel create ROOM_ID \
  --name events \
  --kind message \
  --content-type application/json

beam room channel list ROOM_ID
beam room channel inspect ROOM_ID CHANNEL_ID
beam room channel publish ROOM_ID CHANNEL_ID --literal '{"ready":true}'
beam room channel listen ROOM_ID CHANNEL_ID
beam room channel close ROOM_ID CHANNEL_ID
```

Supported channel kinds are `message`, `stream`, `datagram`, `request-reply`, `media`, and `object`.

Media channels expose a WHIP ingest endpoint for an encoder such as OBS, and a
player URL for viewing:

```console
beam room channel media publish ROOM_ID CHANNEL_ID
beam room channel media publish ROOM_ID CHANNEL_ID --as studio-cam
beam room channel media list ROOM_ID CHANNEL_ID
beam room channel media view ROOM_ID CHANNEL_ID
beam room channel media view ROOM_ID CHANNEL_ID --session WORKLOAD_ID
beam room channel media view ROOM_ID CHANNEL_ID --all --sdp --open
beam room channel media view ROOM_ID CHANNEL_ID --sdp --open
beam room channel media view ROOM_ID CHANNEL_ID --snippets
```

`publish` addresses one named publisher, defaulting to `main`. The returned WHIP
URL and bearer token are stable: configure the encoder once and they stay valid
across streams and tunnel agent restarts. Publishing again under the same name
returns the same URL and token, and distinct names stream concurrently on one
channel. Each name has its own URL and token: a token issued for one name is
refused on another. The name is the address, so there is no rename; publish
under a new name and the previous one simply stops being used.

`list` reports every publisher currently streaming in the channel, most recently
updated first, with the workload id that addresses each one and whether this
agent is its source. A subscription binds a single publisher, so on a channel
carrying several streams this is what tells a member the others exist.

`view` subscribes to one publisher and returns a player URL served by the local
tunnel agent, plus the WHEP URL and token behind it. `--session` selects a
publisher by the workload id `list` reports; without it the agent binds whichever
publisher reported most recently, which on a busy channel is nobody in
particular. `--all` subscribes to every publisher at once and emits one stream
per publisher — each gets its own session and its own loopback ports, so they
play side by side. `--all` and `--session` are alternatives.

Viewing needs the `subscribe` action on each media channel (plus `discover` on a
restricted channel). A Room owner or a member holding `manage` on the channel
grants it:

```console
beam room grant put ROOM_ID CHANNEL_ID \
  --subject-type member \
  --subject MEMBER_ID \
  --actions discover,subscribe
```

`grant put` replaces the member's action list, so include the actions it
already has.

`--open` hands each stream to the operating system's default handler rather than
to a named application, in a fresh instance per stream: players default to a
single window that the next stream would replace. `--sdp` republishes each
subscribed stream as RTP on loopback and returns a URL serving the session
description as `application/sdp`, plus the same description on disk. Give the URL
to any player that opens a network stream: players dispatch on the media type,
and one handed the bare file may guess from its name instead. Each description is
titled with the publisher it carries, which is what distinguishes the windows
when several are open. Both surfaces are loopback-only and serve this machine,
which `--snippets` reflects: it prints consumer, embed, and publish code marked
with that scope.

A description names one publisher's audio and video. Watching several members
means several descriptions in several player windows; there is no single
description carrying every stream in a channel.

Stream and datagram adapters expose their transport-specific commands:

```console
beam room channel stream ROOM_ID listen CHANNEL_ID --transport auto
beam room channel stream ROOM_ID publish CHANNEL_ID --stdin --transport auto

beam room channel datagram ROOM_ID listen CHANNEL_ID --transport quic
beam room channel datagram ROOM_ID send CHANNEL_ID \
  --literal "event" --ttl-millis 5000 --transport quic
```

Object publication reads a local regular file through the daemon:

```console
beam room channel object publish ROOM_ID CHANNEL_ID --file ./artifact.bin
beam room channel object list ROOM_ID CHANNEL_ID
beam room channel object status ROOM_ID CHANNEL_ID PUBLICATION_ID
beam room channel object cancel ROOM_ID CHANNEL_ID PUBLICATION_ID
```

Object publication defaults to strict completion: every selected recipient must
finish. Add `--allow-partial` only when delivery to a subset is acceptable.
With `--follow`, a failed, cancelled, expired or rejected publication exits
nonzero after showing each recipient's status; explicit partial completion is
shown as `partial` and exits zero. `status` remains an inspection command.

Use `--to` repeatedly to include agent and bucket members in the same frozen
recipient set. An authorized delegate can publish an existing bucket object as
the source:

```console
beam room channel object publish ROOM_ID CHANNEL_ID \
  --from BUCKET_MEMBER_ID --object-key archive/source.bin \
  --to AGENT_MEMBER_ID --to OTHER_BUCKET_MEMBER_ID --follow
```

Entirely agent-only publications retain room MLS E2EE. If a publication includes
any bucket, every leg uses TLS with worker-visible plaintext, including its
agent recipients. Orchestrators assign source ranges to workers; each worker
reads its chunk once and reuses that buffer across the frozen recipients.
Neither the source agent nor Studio performs provider transfers. Workers get
short-lived routes and scoped agent assignments, never storage credentials.

The existing `object status` and `--follow` displays show the Runtime transfer
identity, coordinator-frozen protection, standard chunk size, and recipient
coverage. CLI publication and cancellation use the same Runtime lifecycle as
Studio. Terminal storage status JSON includes the Runtime execution evidence
returned by Studio, without provider routes or agent access tokens.

### R2 source range limitation

Some completed R2 multipart objects with gaps in their part numbers return a
truncated body for a range that crosses an upload-part boundary, even though a
full download has the correct contents. Reusing such an object as a source can
fail closed with a source-length error. A successful full-file checksum does
not establish that all partial reads work. Until the provider issue is resolved,
use independently uploaded, range-verified source objects. Beam retains its
standard chunk policy and multipart attempt-slot numbering.

## Local persistence

Persistence defaults to `none`, which is live only: a message reaches only the
members that are listening when it is published. Nothing fails when nobody is
listening; the message is simply never delivered to them, including a peer
that runs `listen` a moment later. Start `listen` on the receiving side first,
or use local retention. After a `--retention none` publish, `beam room` notes
on stderr when nobody received the message, when members were skipped
because they were not listening, or when the delivery failed for a member that
was listening, and `listen` reminds you that earlier messages are not
replayed. `--json` and `--quiet` stay silent. `listen` also
fails instead of exiting silently when its stream ends before any message,
which is how a missing `subscribe` grant shows up.

A channel that allows local retention must declare its modes and positive
bounds:

```console
beam room channel create ROOM_ID \
  --name durable-events \
  --kind message \
  --content-type application/json \
  --persistence-modes none,sender_local,receiver_local \
  --persistence-max-bytes 1048576 \
  --persistence-max-age 1h
```

Inspect and operate the agent-local outbox or inbox with:

```console
beam room channel persistence ROOM_ID CHANNEL_ID status --kind outbox
beam room channel persistence ROOM_ID CHANNEL_ID retry --record RECORD_ID
beam room channel persistence ROOM_ID CHANNEL_ID acknowledge --record RECORD_ID
beam room channel persistence ROOM_ID CHANNEL_ID purge --kind inbox
```

Payloads, local encryption keys, backend configuration, and retained records
remain agent-local; the coordinator receives only control metadata.

## Troubleshooting

`beam room` explains the coordinator refusals it recognises and prints a hint.
With `--json`, the error object carries a stable `kind`:

| Kind | Cause | What to do |
| --- | --- | --- |
| `room_create_requires_account_agent` | The machine joined through a Room invitation and cannot create Rooms. | `beam agent disconnect`, then `beam agent connect`. |
| `room_api_key_permission` | The API key's role lacks `rooms:start`. | Use a key whose role grants `rooms:start`. |
| `room_api_key_required` | No API key was sent to pay for the Room. | Pass `--api-key` or set `BEAM_API_KEY`. |
| `room_credit_required` | The key cannot pay right now (organization credit, key cap, or budget). | Check the key and organization in the Beam Console. |
| `room_billing_unavailable` | Beam could not check credit. | Retry shortly. |
| `channel_not_ready` | The channel's encryption (MLS) is not ready for this member. The hint lists the likely causes; the setup phase the agent reports can lag behind a join that already happened. | Retry in a moment, then follow the hint. |
| `channel_not_visible` | The channel does not exist, or it is restricted and this member lacks `discover` on it (publish and subscribe grants do not help without it). | Check the ID with `beam room channel list ROOM_ID`; a Room owner grants `discover`. |
| `channel_grants_not_visible` | `grant` commands on a channel need `manage` on it or the Room owner role; for other members the channel looks missing. | Ask a Room owner. |
| `mls_controller_unavailable` | No online member agent holding `manage` could be elected to run the channel's encryption. | A Room owner grants `manage` to a member agent that stays online. |
| `mls_no_manager_agent` | No member agent holds `manage` on the channel. | A Room owner grants `manage` to a member agent; an organization owner or Studio member cannot act as controller. |
| `mls_managers_offline` | Every member agent with `manage` is offline. | Start one of them, or have a Room owner grant `manage` to an online agent. |
| `channel_permission_denied` | This member lacks the channel action it used (`publish`, `subscribe`, ...). | Ask a Room owner for the grant. |
| `room_permission_denied` | The member's roles and grants do not allow a Room or channel change. | Ask a Room owner; changes need `manage` or the owner/admin role. |
| `channel_listen_closed` | `listen` ended before any message: missing `subscribe` or channel not ready. | Follow the hint. |
| `channel_no_recipient` | No other member was online and allowed to receive the message. | Start the receiver, then publish again. |
| `channel_not_delivered` | The message did not reach every member, typically because they were not listening. The message says "No member received this message" only when the delivery counts show none did and none of the deliveries failed. | Start `listen` first, or use local retention. |
| `channel_partially_delivered` | Some members received the message; the others were not listening and never will. | Start `listen` on every receiver first, or use local retention. |
| `channel_delivery_failed` | A member was listening, but the delivery to it failed (for example, its agent could not decrypt the message). The message counts the failed members, names them and the cause when the agent reports them, and separately counts members that received the message or were not listening. | Publish again; if it keeps failing, run `beam agent logs` on the receiving member's machine. |
| `channel_setup_timeout` | No Beam Worker picked up the delivery in time (or, after one did, the transfer stalled). | Retry; if it persists, the network may have no Worker available. |
| `worker_capability_unavailable` | No Beam Worker currently offers what the command needs, for example message delivery. | Retry later. |
| `agent_capability_unavailable` | A member agent involved does not support the operation or is offline. | `beam update` and start the agent on that machine. |
| `coordinator_timeout` | The agent's request to the coordinator timed out (for example under load). The message names the request without its query string. | Retry; check `beam agent status`; if it persists, check network access to the coordinator. |
| `coordinator_unreachable` | The agent could not reach the coordinator (connection refused or reset, or closed before a response). | Retry; check `beam agent status`; if it persists, check network access to the coordinator. |

Grant commands (`grant list`, `grant put`, `grant revoke`) need `manage` on the
channel or the Room owner role. Hints that suggest one say so; a plain member
passes it on to a Room owner.

When a channel is not ready, `beam room` first checks that this member can see
the channel, then reads a controller failure the agent reports for it, and only
then falls back to this machine's MLS setup phase, to tell a hidden channel, a
missing or offline `manage` holder, and a missing grant apart.

## Output and compatibility

Use `--json` for structured output and `--quiet`/`-q` for the primary resource
identifier. For example:

```console
ROOM_ID=$(beam room create --quiet)  # BEAM_API_KEY names the payer
beam room inspect "$ROOM_ID" --json
```

```console
beam room list --json
```
