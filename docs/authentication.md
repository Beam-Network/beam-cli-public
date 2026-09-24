# Central CLI authentication

`beam auth` is the only CLI namespace that owns user authentication:

```text
beam auth login
beam auth logout
beam auth whoami
```

Beam CLI is an OAuth public client. Its fixed identity is `beam-cli`, its only
scope is `cli:access`, and it has no client secret. Production defaults are
`https://auth.b1m.ai` for Beam Auth and `https://api.b1m.ai` for Beam API.
Development builds instead use `https://auth.b1m.ai` and
`https://api.b1m.ai`.

## Device flow

`beam auth login` posts URL-encoded fields to `/oauth/device/authorize`, opens
`verification_uri_complete` when interactive, and always displays the user
code and verification URL. It then polls `/oauth/token` with the standard OAuth
device grant type.

Polling honors the server interval. `authorization_pending` continues,
`slow_down` adds at least five seconds, transient network and 5xx failures use
bounded exponential backoff, and denial, expiration, or `invalid_grant` ends
the attempt. Cancelling the command interrupts its timer immediately. Only the
OAuth `snake_case` response fields are accepted; retired camelCase responses
and `/api/auth/device/*` endpoints are not supported.

Interactive terminals show one in-place spinner. Redirected human output gets
one static status line. JSON and quiet modes do not open a browser or render a
spinner. JSON mode writes a structured `device_authorization` event to stderr
and the final result to stdout.

## Refresh and Beam API calls

The RS256 access token is short-lived and remains in memory. Before it expires,
the CLI exchanges the durable refresh token at `/oauth/token`. Every successful
exchange replaces the previous refresh token before the new access token is
used. If that atomic commit fails, the CLI deletes the session rather than
reusing the consumed generation.

The Beam API client centrally adds `Authorization: Bearer ...`. After login and
for `beam auth whoami`, it loads the profile from `/api/me` and organizations
from `/api/organizations`. A 401 triggers exactly one forced refresh and one
request replay. A second 401 expires the local session. Network errors and 5xx,
including 503, preserve the refresh token so the command can be retried.

## Credential storage

The rotating refresh token is stored in the OS credential store when available:

- macOS Keychain;
- Windows Credential Manager;
- Linux Secret Service.

If the OS service is unavailable, Beam uses `credentials.json`, written by
atomic replacement with mode `0600` inside a `0700` directory on Unix. The file
may also contain non-secret cached profile and organization data. Access tokens,
refresh tokens, and device codes are never logged or printed. They are never
written to `.env` files or the repository.

Credential format version 1 and the original single-access-token format are
invalidated and removed on read. There is no automatic fallback to the Phase 1
HS256 session flow.

## Logout

`beam auth logout` posts the current refresh token to `/oauth/revoke`, then
deletes all local credentials even if Beam Auth cannot be reached. If network
revocation cannot be confirmed, the CLI reports that fact without retaining
the local token.

Registry commands consume the same OAuth session and refresh boundary. They do
not implement separate login, logout, or identity commands.
