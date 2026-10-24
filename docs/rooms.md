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

`status` must report a registered, connected agent and `Room control: healthy`.
Room commands do not own a second login or identity and do not start a
separate service.

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
local enrollment work. When an invitation cannot be used, `join` says why
(expired, used up, revoked, not valid for this Room, not found, Room closed,
or the Room is not taking joins right now) and what to do, with a stable
`kind` in `--json` errors; see [Troubleshooting](#troubleshooting). On Unix,
invitation files must be private; use `chmod 600 invitation.token` when
prompted.

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
beam room ROOM_ID member list
```

The listing includes agents, object-storage buckets, and
[webhook members](#webhook-members), with each member's kind (`TYPE` in
`members`, `KIND` in `member list`). Bucket membership is attached in Studio
from an existing organization credential; the CLI shows its room identity,
display name, availability, roles, and grants without ever receiving the
storage credential.

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

A stream channel carries bytes of unknown length from one publisher to the
members listening. `publish` reads stdin until it closes, and `listen` writes
the bytes to stdout:

```console
beam room channel stream ROOM_ID listen CHANNEL_ID > received.bin
producer | beam room channel stream ROOM_ID publish CHANNEL_ID
```

Beam Workers serve streams directly, and every stream is end-to-end encrypted
with the channel's key: a Worker sees sizes and timing, never the plaintext. Beam
picks the Worker, so stream commands take no relay or transport flags. A stream
runs on one-hour Worker leases that renew while it continues. Only members
already running `listen` when a stream starts receive it. `listen` exits 0 only
after the stream's authenticated end; a stream cut short, or one that fails
after output started, exits non-zero with the cause (for example
`stream_truncated`), even though the bytes before it were written. Every member
involved needs a Beam CLI and agent recent enough to support Worker-served
streams.

The datagram adapter exposes its transport-specific commands:

```console
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
With `--follow`, `publish` shows each recipient's status until the publication
ends. A publication that ends `failed`, `cancelled`, `expired` or `rejected`,
or `partial` without `--allow-partial`, exits 10 (operation failed) with kind
`object_publication_<state>`, the reason the coordinator gives, and each
recipient that missed the file with the reason its delivery gives, for example
`It failed for 1 of 2 recipients: btr_member_x (agent_offline).`; under
`--allow-partial`, `partial` exits zero. If following the publication fails,
for example because the agent stops answering, `publish` exits 10 with kind
`object_publication_follow_failed`; the publication may still be delivering, so
follow it with `object watch PUBLICATION_ID`. `beam` never publishes a file
again on its own: running `publish` again creates a new publication that sends
the file to every selected recipient again, so reach only the recipients that
missed it with `--to`. `status` remains an inspection command.

A publication's lifetime, `--ttl` in seconds, defaults to the channel's
publication TTL less 5 seconds, which absorbs clock differences with the
coordinator. `channel create` sets that TTL with `--publication-ttl` (one
minute unless set). The agent refuses a publication that outlives the channel's
publication TTL, so `publish` reads the channel first and refuses a longer
`--ttl` before anything is published (kind `object_ttl_exceeds_channel`). A Room
owner or a member holding `manage` on the channel raises the limit with
`channel update`, which replaces the channel's whole policy, so repeat any other
flag the channel was created with:

```console
beam room channel update ROOM_ID CHANNEL_ID \
  --name files --kind object --publication-ttl 15m
```

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
that runs `listen` a moment later. A publish that reached nobody succeeds only
when every member's agent said it was not listening: when a delivery failed, a
member's agent never answered, or a membership change re-keyed the channel
while the message was in flight, it exits non-zero
(`channel_delivery_failed`, `channel_delivery_unconfirmed` or
`channel_rekeying`). `channel_rekeying` means nothing was sent. A member whose
agent never answered may still have the message, so
`channel_delivery_unconfirmed` names those members and never says that
nothing was sent: publishing again can deliver it twice. The agent publishes
a message again by itself only when no member can have it: the channel
re-keyed before the message left this machine, every member refused it as
stale, or no member's agent had a free path for it. The publish result then
names a new `publication_id`. Start `listen` on the receiving side first, or
use local retention. After a `--retention none` publish, `beam room` notes on
stderr when nobody received the message, when members were skipped because
they were not listening, when the delivery failed for a member that was
listening, or when members never answered, naming the members that received
it and those that did not, and `listen` reminds you that earlier messages are
not replayed. `--json` and `--quiet` stay silent. `listen` also fails instead
of exiting silently when its stream ends before any message, which is how a
missing `subscribe` grant shows up.

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

## Webhook members

A webhook member lets any system that receives HTTPS webhooks take part in a
Room. It is a visible member with its own channel grants, run by another
member's always-on agent: its **host**. The host decrypts what the webhook is
sent, POSTs it to the webhook URL, and publishes the receiver's replies as the
webhook. Beam infrastructure never decrypts webhook traffic.

**Trust model.** The host can read everything sent to the webhook and can
publish in the Room as the webhook. The URL and the signing secret exist only
on the host; the coordinator sees the URL's host name and a fingerprint.

### Add, accept, deliver

1. A Room owner or admin adds the webhook and names its host, an agent member
   of the Room:

   ```console
   beam room ROOM_ID webhook add \
     --name alerts \
     --host HOST_MEMBER_ID \
     --channel EVENTS_CHANNEL_ID=discover,subscribe,publish
   ```

   `--channel` is repeatable. A webhook publishes its replies on the channel of
   the message that triggered them, so it needs `publish` on each channel whose
   messages it may answer; leave `publish` out for a webhook that only
   receives. A webhook holds only `discover`, `subscribe`, and
   `publish` on message channels, and `discover` and `subscribe` on object
   channels: never `manage`, roles, or other channel kinds. `--payload-mode raw`
   POSTs the published body unchanged instead of a JSON envelope.
   `--max-file-mb` (1 to 25, default 25) caps the files sent with their bytes;
   a larger file is announced by its metadata only. `--quiet` prints the new
   webhook's member ID, and `--idempotency-key` makes a retried `add` safe. The
   webhook stays `pending` until its host accepts it.

2. On the host's machine, its owner lists what the machine is asked to host and
   accepts the webhook with its URL and signing secret:

   ```console
   beam webhook hosted
   chmod 600 webhook.secret
   beam room ROOM_ID webhook accept WEBHOOK_ID \
     --url https://hooks.example.com/beam \
     --secret-file webhook.secret
   ```

   `accept` shows the trust model and asks for confirmation; pass `--yes` when
   no one can answer. The secret (16 to 512 bytes, the receiving system's
   shared secret) comes from a private file or from standard input with
   `--secret-stdin`, typed without echo on a terminal. It is never accepted as
   a command-line value and never printed. The URL must use `https`;
   `--allow-private` lets it reach a private address, such as a service on the
   host's network. `accept` also takes `--payload-mode` and `--max-file-mb`.
   `beam room ROOM_ID webhook decline WEBHOOK_ID` refuses the webhook instead,
   which removes it.

3. The host delivers. Send a signed test event to check the receiver:

   ```console
   beam room ROOM_ID webhook test WEBHOOK_ID
   ```

   `test` reports the outcome and exits non-zero unless the receiver accepted
   the request. It does not change the webhook's state.

A webhook receives what is published while its host's agent is online, and
nothing is kept for it while the host is offline. A message channel granted to
a webhook must therefore allow retention `none`. Every request carries
`X-Beam-Timestamp` and
`X-Beam-Signature: v1=<hex HMAC-SHA256(secret, "v1:<timestamp>:<body>")>`; the
receiver checks the signature with the shared secret and rejects stale
timestamps.

### Replies

A receiver replies in its response: `X-Beam-Reply: v1` and a JSON body of at
most 64 KiB, within the 10-second attempt. The host publishes it as the
webhook on the trigger's channel, one hop after the trigger and referencing
it. A receiver that needs longer answers `202 Accepted` with
`X-Beam-Reply: v1` and a `Location` on the webhook URL's origin. The host then
polls that `Location` with signed `GET` requests (the signature covers an
empty body) at the receiver's `Retry-After`, or every 30 seconds, until it
answers:

- `202` again: still pending;
- `2xx` with `X-Beam-Reply: v1` and a JSON body: the reply, published as above;
- `204` or another `2xx`: finished without a reply;
- `5xx`, `401`, `403`, `408`, `429`, a network error or a timeout: polled
  again;
- `404`, `410`, a redirect or any other `4xx`: the host stops polling.

The host keeps polling through restarts until the reply window ends, one hour
by default. Change it at `accept` with `--reply-window`, or later:

```console
beam room ROOM_ID webhook reply-window WEBHOOK_ID 6h
```

The window is 1m to 24h. A new one applies to later `202` responses; replies
already polled for keep their window. `beam webhook hosted` counts the replies polled for. A paused webhook
does not poll, and a webhook that loses access to the channel stops polling and
publishes nothing more.

### Durable delivery

Without durable delivery, the host retries an event for a few seconds and then
drops it. With it, the host keeps each event of a channel that also allows
`receiver_local` retention on its machine, encrypted, before accepting it, and
retries it for up to 24 hours: at most 5 minutes apart, or as long as the
`Retry-After` of a 429 or 503 response asks. It survives restarts of the host
agent and always carries the same `Idempotency-Key`. The 24 hours keep running
while the webhook is paused. Turn it on at `accept` with
`--durable`, or later:

```console
beam room ROOM_ID webhook durable WEBHOOK_ID on
```

The command says how many channels qualify (`beam webhook hosted --json` lists
them in `durable_channel_ids`); with none, the webhook's events stay live. Right after an `accept`, before the webhook runs, `beam webhook hosted`
shows them a few seconds later. Turning it `off` keeps nothing new, and the events already kept are still
delivered. `beam webhook hosted` shows the pending events, the dead letters and
the space they use. Every Room member sees the setting in `beam room ROOM_ID
webhook show WEBHOOK_ID` (`Durable: on` or `off`) when the coordinator reports
it; a host agent too old to report the setting shows `off`, and an agent
started before its coordinator could take the report sends it from its next
start.

An event that keeps failing past its 24 hours, or that the receiver rejects for
good (a redirect, or a 4xx other than 401, 403, 408, 410 or 429), becomes a dead
letter, kept for 7 days. A 410 pauses the webhook instead, and the event waits
for a resume within its 24 hours. A channel's max age counts from arrival and
caps both periods, so the full 24 hours and 7 days need a max age of at least
192h.

```console
beam room ROOM_ID webhook dead-letters list WEBHOOK_ID
beam room ROOM_ID webhook dead-letters replay WEBHOOK_ID PUBLICATION_ID
beam room ROOM_ID webhook dead-letters purge WEBHOOK_ID --all
```

The host agent keeps 1024 MiB by default (`BEAM_TUNNEL_WEBHOOK_SPOOL_MAX_MIB` in
its environment), shared equally among the webhooks that deliver durably at
that moment, and a channel's max bytes bounds its events. Dead letters count
toward the space. A publish past either limit lists the webhook in `member_failures` with
`webhook_spool_full`. A webhook that loses access to a channel has that
channel's kept events and dead letters deleted at once.

### States

`beam room ROOM_ID webhook list` shows each webhook's state and reason, and
whether it delivers now (`DELIVERY`); `show` explains what the state means and
what to do:

| State | Meaning | What to do |
| --- | --- | --- |
| `pending` | Waiting for its host to accept it with the URL and secret. | Accept on the host's machine. |
| `active` | Delivering. | |
| `degraded` | Still delivering, after 5 failed delivery attempts in a row (`failing`) or a rejected signature (`auth`, HTTP 401 or 403). After `failing`, its host sends one attempt at a time. Failures never pause a webhook. | Fix the receiver, or rotate the secret. A successful delivery returns it to `active`. |
| `paused` | Not delivering: an HTTP 410 response (`gone`), or its host left or was removed (`host_left`, `host_removed`). | `resume` it, or `move` it when it has no host. |

A webhook view also carries two presences, `online` or `offline` as `member
list` shows them: `presence`, the webhook member's own, and `host_presence`,
its host member's. Publishes reach a webhook only while it is online, so an
`active` or `degraded` webhook delivers nothing while either is offline, and
keeps its state:

- While the host's agent is offline, `list` shows `host offline: not
  delivering` and `show` says `Host offline: not delivering.` This appears
  when `member list` shows the host offline, once its agent's Room lease
  expires, and delivery resumes when the host's agent is back online.
- While the host is online but the webhook is not, `list` shows `webhook
  offline: not delivering` and `show` says `Webhook offline: not delivering.`
  The host's agent brings the webhook online seconds after it starts or
  accepts the webhook; `beam webhook hosted` on the host's machine shows
  whether its agent runs it.
- While the webhook is online but does not hold the current encryption keys
  of some of its channels yet, `list` shows `joining encryption: not
  delivering on N channels` and `show` says `Joining the encryption of N
  channels: not delivering on them yet`, naming them. The view lists those
  channels in `keys_pending_channel_ids`. After an accept, a host move or
  another membership change the channel re-keys and the host confirms the new
  keys within seconds; only then does `show` say `Delivering.`

### Members that must upgrade

A webhook gets a channel's keys only from the member whose agent controls that
channel's encryption: one of the members with `manage` on it. Agents released
before webhook members never hand keys to a webhook, so every member that
manages a channel a webhook publishes or subscribes to must run an up-to-date
agent (`beam update`). Older agents still join Rooms as before.

While such a member manages one of those channels, adding the webhook,
accepting it, and granting it `publish` or `subscribe` on another channel fail
with `webhook_controller_upgrade_required`; the message names the members. A
member that gets `manage` after the webhook was added is not refused: `show`
then prints, without changing the state,

```text
Blocked: member btr_member_... runs an agent without webhook support and must upgrade beam.
```

and `list` and `beam webhook hosted` print a `Blocked webhook ...` line under
the table. With `--json`, the webhook carries the members in
`upgrade_required_member_ids`. Once they upgrade, or lose `manage` on the
webhook's channels, the line disappears.

### Resume, move, rotate, remove

```console
beam room ROOM_ID webhook resume WEBHOOK_ID
beam room ROOM_ID webhook move WEBHOOK_ID --host OTHER_MEMBER_ID
beam room ROOM_ID webhook rotate-secret WEBHOOK_ID --secret-file new.secret --overlap 24h
beam room ROOM_ID webhook remove WEBHOOK_ID
```

- `resume` returns a paused or degraded webhook to `active`. A Room owner or
  admin can resume it, and so can its host. A webhook without a host cannot be
  resumed: move it first.
- `move` names a new host. The webhook returns to `pending`, its channel keys
  rotate so the previous host cannot read what is sent afterwards, and the new
  host accepts it with the URL and secret again. Moving is also how an accepted
  webhook gets a new URL.
- `rotate-secret` runs on the host. For the `--overlap` (default 24h, at most
  168h; `0` ends it at once), requests are signed with both the new and the
  previous secret, so the receiving system can switch without rejecting
  requests.
- `remove` revokes the webhook member.

A Room holds at most 10 webhooks, and one host agent at most 20 across Rooms,
pending ones included. Adding, moving, and removing a webhook needs the Room
owner or admin role.

### Bridges between Rooms

A receiver that re-publishes a delivery into another Room is a bridge. Its
re-publish is a new message, which starts at hop 0 unless the bridge says
otherwise, so two bridges in opposite directions loop forever. Publish at the
delivery's `X-Beam-Hop` plus one:

```console
beam room channel publish ROOM_B CHANNEL_ID --literal "$TEXT" --hop $((HOP + 1))
```

A message at hop 2 or more reaches no webhook, so the loop stops. `--hop`
takes 0 to 16 and publishes live only.

### Hosts

A host is an active agent member of the Room whose agent is connected to a
Beam account (`beam agent connect`). A machine that joined only through an
invitation, or a Studio-managed agent, cannot host. `beam webhook hosted` lists
the webhooks this machine hosts or is asked to host, across Rooms, with their
state, the host name of their URL, and whether this machine is delivering them;
`--json` adds the local delivery state, including recent deliveries.

Webhook commands first check that this machine's agent offers them: an agent
too old for webhook members, or whose coordinator does not support them,
fails with `webhook_manage_unavailable` (or `webhook_host_unavailable` for the
host commands) before anything is sent.

## Troubleshooting

`beam room` explains the coordinator refusals it recognises and prints a hint.
With `--json`, the error object carries a stable `kind`:

| Kind | Cause | What to do |
| --- | --- | --- |
| `room_create_requires_account_agent` | The machine joined through a Room invitation and cannot create Rooms. | `beam agent disconnect`, then `beam agent connect`. |
| `room_api_key_required` | No API key was sent to pay for the Room. | Pass `--api-key` or set `BEAM_API_KEY`. |
| `room_api_key_invalid` | Beam refused the API key: it is unknown, expired, revoked or disabled. | Renew or replace the key in the Beam Console. |
| `room_permission_required` | The API key's role lacks the permission the command needs, such as `rooms:start`. | Use a key whose role grants that permission. |
| `room_insufficient_credits` | The key cannot pay right now. The message gives the credits required and available and the reason: `pool_exhausted` (organization credit), `key_cap_exhausted` (the key's credit limit) or `key_budget_blocked` (the key's monthly budget). | Add credits, or raise the key's credit limit or monthly budget, in the Beam Console. |
| `room_organization_blocked` | The organization that owns the key cannot run billable actions. | An owner of the organization contacts Beam support. |
| `room_billing_unavailable` | Beam could not check credit. | Retry shortly. |
| `invitation_expired` | `join`: the invitation has expired. | Ask the Room owner for a new invitation. |
| `invitation_exhausted` | `join`: the invitation is used up. | Ask the Room owner for a new invitation. |
| `invitation_revoked` | `join`: the Room owner revoked the invitation. | Ask the Room owner for a new invitation. |
| `invitation_unusable` | `join`: the invitation is used up or was revoked. | Ask the Room owner for a new invitation. |
| `invitation_invalid` | `join`: the invitation is not valid for this Room: a mistyped or incomplete token, another Room's invitation, or one created for another agent or account. | Check the Room ID and the invitation token. |
| `invitation_not_found` | `join`: the invitation or its Room does not exist. | Check the Room ID and the invitation token, or ask for a new invitation. |
| `room_closed` | `join`: the Room is closed. | Ask the Room owner for an invitation to an active Room. |
| `room_join_limited` | `join`: the Room is full, or too many join attempts were made. | Wait a moment and try again. |
| `channel_not_ready` | The channel's encryption (MLS) is not ready for this member. The hint lists the likely causes; the setup phase the agent reports can lag behind a join that already happened. | Retry in a moment, then follow the hint. |
| `channel_rekeying` | A membership change (a member joining or leaving, a webhook accepted or moved) is renewing the channel's encryption keys, and the agent already waited up to 30 seconds for them, or the channel kept re-keying while a live-only message was in flight so that no member could open it. Nothing was sent. The hint says how long this machine's agent has been re-keying. | Retry in a moment; if it lasts more than a minute, check that a member agent holding `manage` on the channel is online. |
| `channel_not_visible` | The channel does not exist, or it is restricted and this member lacks `discover` on it (publish and subscribe grants do not help without it). | Check the ID with `beam room channel list ROOM_ID`; a Room owner grants `discover`. |
| `channel_grants_not_visible` | `grant` commands on a channel need `manage` on it or the Room owner role; for other members the channel looks missing. | Ask a Room owner. |
| `mls_controller_unavailable` | No online member agent holding `manage` could be elected to run the channel's encryption. | A Room owner grants `manage` to a member agent that stays online. |
| `mls_no_manager_agent` | No member agent holds `manage` on the channel. | A Room owner grants `manage` to a member agent; an organization owner or Studio member cannot act as controller. |
| `mls_managers_offline` | Every member agent with `manage` is offline. | Start one of them, or have a Room owner grant `manage` to an online agent. |
| `channel_permission_denied` | This member lacks the channel action it used (`publish`, `subscribe`, ...). | Ask a Room owner for the grant. |
| `room_permission_denied` | The member's roles and grants do not allow a Room or channel change. | Ask a Room owner; changes need `manage` or the owner/admin role. |
| `channel_listen_closed` | `listen` ended before any message: missing `subscribe` or channel not ready. | Follow the hint. |
| `object_ttl_exceeds_channel` | `object publish --ttl` is longer than the channel's publication TTL; nothing was published. The message names both values. | Omit `--ttl`, or have a Room owner or a member holding `manage` on the channel raise its limit with the `channel update` command the hint prints. |
| `object_publication_partial` | A followed `object publish` reached some selected recipients but not all, without `--allow-partial`. The message names the recipients that missed the file and why. | `object show PUBLICATION_ID` lists each recipient's state; publish again with `--to` for the ones that missed it. |
| `object_publication_failed`, `object_publication_rejected` | A followed `object publish` failed or was refused. The message gives the coordinator's reason when there is one, and names the recipients that missed the file and why. | Fix the cause, then publish again; that sends the file to every selected recipient again. |
| `object_publication_cancelled`, `object_publication_expired` | A followed `object publish` was cancelled, or its lifetime ran out, before every recipient received the file. | `object show PUBLICATION_ID` lists each recipient's state. |
| `agent_request_interrupted` | The request connected to this machine's agent, but the connection broke or timed out before the agent replied, so the agent may have carried it out. `beam` does not run it again. For `object publish`, the publication may exist and be delivering. | Check whether it took effect before running it again; for `object publish`, find the publication with `object list` and follow it with `object watch PUBLICATION_ID`. |
| `object_publication_follow_failed` | `object publish --follow` submitted the publication, then could not read its status, for example because the agent stopped answering. | Follow it with `object watch PUBLICATION_ID` rather than publishing again. |
| `channel_no_recipient` | No other member was online and allowed to receive the message. | Start the receiver, then publish again. |
| `channel_not_delivered` | The message did not reach every member, typically because they were not listening. The message says "No member received this message" only when the delivery counts show none did and none of the deliveries failed. | Start `listen` first, or use local retention. |
| `channel_partially_delivered` | Some members received the message; the others were not listening and never will. | Start `listen` on every receiver first, or use local retention. |
| `channel_delivery_unconfirmed` | No member confirmed receiving a live-only message, and the agents of some members never answered, for example because the channel re-keyed while the message was in flight: they may have it. The message names them, and the members whose delivery failed. | Publishing again can deliver it twice to members that did receive it. If it keeps happening, check that their agents are running and connected (`beam agent status` on their machines). |
| `channel_delivery_failed` | A member was listening, but the delivery to it failed (for example, its agent could not decrypt the message). The message counts the failed members, names them and the cause when the agent reports them, and separately counts members that received the message or were not listening. | Publish again; if it keeps failing, run `beam agent logs` on the receiving member's machine. |
| `channel_setup_timeout` | No Beam Worker picked up the delivery in time (or, after one did, the transfer stalled). | Retry; if it persists, the network may have no Worker available. |
| `worker_capability_unavailable` | No Beam Worker currently offers what the command needs, for example message delivery or streams. | Retry later. |
| `datagram_relay_unavailable` | A datagram send failed before anything was sent: datagram channels currently depend on Beam relays, and none was available. | Retry later. |
| `agent_capability_unavailable` | A member agent involved does not support the operation or is offline. For a stream, a member's agent does not support Worker-served streams (`room.stream.direct.v1`). | `beam update` on every member's machine, and start the agent there. |
| `stream_key_unavailable` | This machine could not get the channel's current encryption key for a stream: the key is rotating, encryption setup is still finishing, or the member left the Room. | Retry in a moment. |
| `stream_renewal_failed` | A stream stopped because Beam could not renew its one-hour Worker lease. The message names the reason, such as `worker_capability_unavailable`. | Publish the stream again. |
| `no_active_targets` | A stream stopped because every member receiving it dropped out. The message names the reasons, such as `no_live_subscriber` for a member that was not listening. | Start `stream listen` on the receivers, then publish again. |
| `stream_truncated` | A listened stream ended without its end-of-stream marker, so the data written is incomplete. The message names the reason, such as `source_aborted`; without one, the local agent may have stopped. | Listen again before the stream is published again. |
| `room_control_degraded` | The agent's Room control does not reach the coordinator. Either its latest room discovery failed, which blocks every Room command, or the lease of the Room this command names has kept failing for 10 seconds, which blocks only that Room's commands; commands for other Rooms run with a warning. The message names the operation, the error code, and since when. The command did not run. | Check network access to the coordinator; `beam agent status` shows when Room control recovers and `beam agent logs` each failed attempt. |
| `coordinator_timeout` | The agent's request to the coordinator timed out (for example under load). The message names the request without its query string. | Retry; check `beam agent status`; if it persists, check network access to the coordinator. |
| `coordinator_unreachable` | The agent could not reach the coordinator (connection refused or reset, or closed before a response). | Retry; check `beam agent status`; if it persists, check network access to the coordinator. |
| `webhook_manage_unavailable` | This machine's agent, or its coordinator, does not support webhook members. | `beam update`, then `beam agent restart`. |
| `webhook_host_unavailable` | This machine's agent cannot host webhooks: it is too old, not connected to a Beam account, Studio-managed, or its coordinator lacks support. | `beam update`, or have a Room owner `move` the webhook to another member. |
| `webhook_host_requires_account_agent` | This machine joined through an invitation only, so it cannot host. | A Room owner moves the webhook to a member connected to a Beam account. |
| `webhook_host_ineligible` | The named host cannot host webhooks: not an active agent member connected to a Beam account, or its agent lacks hosting support. | Name another member. |
| `webhook_permission_denied` | Adding, moving, removing, or resuming the webhook needs the Room owner or admin role. | Ask a Room owner. |
| `webhook_host_permission_denied` | This machine's agent is not the webhook's host. | Run host commands on the host's machine. |
| `webhook_grant_not_allowed` | A `--channel` grant is outside the webhook limits. | Grant only `discover`, `subscribe`, and `publish` on message channels, and `discover` and `subscribe` on object channels. |
| `webhook_channel_requires_retention` | A granted channel does not allow retention `none`, and a webhook receives live messages only. | Allow `none` on the channel, or leave it out. |
| `webhook_controller_upgrade_required` | A member that manages one of the webhook's channels runs an agent released before webhook members, so the webhook would get no keys for that channel. The message names the members. | Those members run `beam update`, or lose `manage` on the webhook's channels. |
| `webhook_limit_reached` | The Room already holds 10 webhooks, or the host agent 20. | Remove one, or name another host. |
| `webhook_state_conflict` | The webhook's state does not allow the change, such as resuming a webhook without a host. | `show` it, then `move` or `resume`. |
| `webhook_not_found`, `webhook_target_not_found` | The webhook, Room, host member, or channel was not found. | Check the IDs with `webhook list`, `member list`, and `channel list`. |
| `webhook_not_hosted`, `webhook_not_pending`, `webhook_not_accepted` | A host command named a webhook this machine is not asked to host, has already accepted, or has not accepted. | Check `beam webhook hosted`. |
| `webhook_invalid_request` | The URL, secret, payload mode, file cap, or overlap was refused, or the URL host is blocked. | Follow the hint; a private address needs `--allow-private`. |
| `webhook_secret_invalid` | The secret file is missing or readable by other users, or the secret is not 16 to 512 bytes. | `chmod 600` the file, or fix the secret. |
| `webhook_test_failed` | The test event did not reach the receiver successfully. | Follow the hint for the response class. |

A stream whose Worker lease fails for any other reason, or a datagram send that
fails before anything is sent, keeps the agent's message, which names the
reasons the workload failed with.

Grant commands (`grant list`, `grant put`, `grant revoke`) need `manage` on the
channel or the Room owner role. Hints that suggest one say so; a plain member
passes it on to a Room owner.

When a channel is not ready, `beam room` first checks that this member can see
the channel, then reads a controller failure the agent reports for it, then
whether the channel is re-keying, and only then falls back to this machine's
MLS setup phase, to tell a hidden channel, a missing or offline `manage`
holder, a re-key and a missing grant apart. A phase duration counts from the
start of that phase.

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
