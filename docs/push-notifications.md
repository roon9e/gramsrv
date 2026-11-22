# Push notifications (FCM / APNs) — design plan

Status: **proposal / not implemented**. Tracking issue:
[#98](https://github.com/iamxvbaba/gramsrv/issues/98). This document is the
scoped design the issue owner asked for before a PR. FCM is the first target;
APNs is designed but deliberately deferred.

## 1. Summary

Today `telesrv` accepts `account.registerDevice` and returns `true` without
storing anything, and it sends no pushes: a backgrounded client only receives
updates while it keeps an MTProto connection open. This plan adds optional,
**off-by-default** server-side push delivery:

- `account.registerDevice` / `account.unregisterDevice` actually persist device
  tokens and recognise `TOKEN_WAS_INVALIDATED`.
- An optional `TELESRV_PUSH_FCM_*` config block turns on an FCM HTTP v1 sender.
- The durable outbox dispatcher — which already knows whether any live session
  was reached — triggers a push when a user has **no live session**.
- The push payload is the encrypted `p` blob the official clients expect.

With the feature disabled (the default) behaviour is byte-for-byte what it is
now: the RPC still returns `true`, no table is read, no network call is made.

The one unavoidable constraint, stated up front: **the client must be rebuilt
against the operator's own Firebase project** (`google-services.json` on
Android, `GoogleService-Info.plist` + an APNs key on iOS). The server cannot
push to a client that is registered with Telegram's Firebase project.

## 2. Scope

In scope (this plan):

- Android FCM push, token storage, encrypted payload, offline trigger.
- `account.registerDevice` / `unregisterDevice` semantics.
- Config, docs, tests, and a fork-first validation rollout.

Out of scope (documented, not built):

- APNs sender. It needs its own `apns.*` config, an Apple auth key/cert, and a
  distinct payload/rebuild story. The sender interface is designed so APNs is a
  second implementation later.
- Web push, WebSocket push (`updateWebBrowserSettings` area).
- iOS VoIP / `account.registerDevice` VoIP pushes.

## 3. What exists in the code today

| Piece | Location | Note |
|---|---|---|
| `account.registerDevice` returns `true` | `internal/rpc/account.go:37` | no-op |
| `account.unregisterDevice` returns `true` | `internal/rpc/account.go:40` | no-op |
| Durable outbox dispatcher | `internal/rpc/outbox_dispatcher.go` | pushes updates to **online** sessions |
| `pushOutboxUpdate` returns `sent` | `internal/rpc/outbox_dispatcher.go:527` | `sent == 0` ⇒ no live session |
| Session binder | `internal/rpc/deps.go:72` (`SessionBinder`) | online session lookup |
| `IsUserOnline(userID)` | `internal/rpc/deps.go:249` (`OnlineUserProvider`), impl `internal/mtprotoedge/session_manager.go:2103` | raw live-connection index; the canonical "is the device live?" gate |
| Welcome/ephemeral push precedent | `internal/rpc/welcome_delivery_dispatcher.go:98`, `internal/rpc/ephemeral_push.go:62` | "check online, then send" shape |
| Notify settings / mute | `internal/rpc/account_notify.go` | `mute_until` per peer |
| Config type | `internal/config/config.go` | `Enabled bool` + fields, env-parsed |
| Migration naming | `deploy/migrations/<timestamp>_name.{up,down}.sql` | latest `20261117010000_*` |
| `rpc.Deps` injection | `internal/rpc/deps.go` + `deps_validation_test.go` | every field validated |
| Behaviour table | `docs/telegram-differences.md:17` | the push row updates on merge |

### Why the outbox dispatcher, not "enqueue time"

The issue proposes sending "at outbox enqueue time". Enqueue happens inside the
message-write transaction, which has no session state and must stay fast and
side-effect free. The outbox dispatcher is a better hook: it is a separate
worker, it already calls `pushOutboxUpdate` and gets the number of sessions the
update reached, and marking a row delivered with `sent == 0` is exactly the
signal "the user was offline for this event". Triggering there keeps online
delivery untouched and adds no work to the send path.

## 4. Data model and storage

New durable table `device_push_tokens`, keyed by
`(user_id, token_type, token)`, so a re-registration is an upsert and a token
moved between accounts is reassigned. Columns: `user_id`, `token_type` (int,
TL `token_type`), `token` (text), `secret` (bytea, nullable — FCM/APNs VoIP),
`app_sandbox` (bool), `no_muted` (bool), `other_uids` (bigint[]), `created_at`,
`updated_at`, `last_error`/`invalidated_at` for `TOKEN_WAS_INVALIDATED`.

Domain type and store interface follow the passkey/bot-widgets pattern:

```go
// internal/domain/push_token.go
type PushToken struct {
    UserID     int64
    TokenType  int
    Token      string
    Secret     []byte
    AppSandbox bool
    NoMuted    bool
    OtherUIDs  []int64
    UpdatedAt  time.Time
}

// internal/store/push_token.go
type PushTokenStore interface {
    UpsertPushToken(ctx context.Context, token domain.PushToken) error
    DeletePushToken(ctx context.Context, userID int64, tokenType int, token string) (bool, error)
    ListPushTokensForUser(ctx context.Context, userID int64) ([]domain.PushToken, error)
    DeletePushTokenByValue(ctx context.Context, tokenType int, token string) (bool, error) // TOKEN_WAS_INVALIDATED / unregister-other-uid
    ListPushTokensForUsers(ctx context.Context, userIDs []int64) (map[int64][]domain.PushToken, error)
}
```

- **memory** backend in `internal/store/memory` (fast RPC tests).
- **postgres** backend in `internal/store/postgres/push_token.go` with a small
  `queries/push_tokens.sql` + sqlc regeneration.
- Migration `deploy/migrations/<timestamp>_device_push_tokens.up.sql` + `.down.sql`.
- Injected as `rpc.Deps.PushTokens`; `deps_validation_test.go` updated.

## 5. RPC behaviour

`internal/rpc/account.go`:

- `registerDevice`: validate `token_type` (FCM/APNs family), reject empty token
  with `TOKEN_EMPTY`; when `deps.PushTokens == nil` (feature off) return `true`
  exactly as today. Otherwise upsert and, on a token reassigned from another
  account, return `TOKEN_WAS_INVALIDATED`-style handling as the clients expect.
- `unregisterDevice`: delete the `(user, token_type, token)` row; no-op when
  the store is absent.
- `other_uids` are not device rows; they are informational and only stored.

`token_type` handling is table-driven so an unknown type is ignored (stored, not
pushed) rather than erroring, since layers differ.

## 6. Configuration

Off by default; a single `Enabled` gate. Mirrors the `TELESRV_*` table style in
`docs/configuration.en.md` §9 and `.env.example`:

| Setting | Default | Meaning |
|---|---|---|
| `TELESRV_PUSH_FCM_ENABLED` | `false` | Master switch. |
| `TELESRV_PUSH_FCM_SERVICE_ACCOUNT_FILE` | `` | Path to the Firebase service-account JSON (file-backed secret, like the Telegram Login key files — never in `.env`). |
| `TELESRV_PUSH_FCM_PROJECT_ID` | `` | Firebase project id (falls back to the JSON's `project_id`). |
| `TELESRV_PUSH_FCM_TOKEN_URL` | `https://oauth2.googleapis.com/token` | Override for tests/proxies. |
| `TELESRV_PUSH_FCM_SCOPE` | `https://www.googleapis.com/auth/firebase.messaging` | OAuth2 scope. |
| `TELESRV_PUSH_DEBOUNCE` | `30s` | Per-user coalescing window. |
| `TELESRV_PUSH_SEND_TIMEOUT` | `5s` | Per-request HTTP timeout. |

Every new key must also be added to `.env.example`: `internal/config/drift.go`
(and its `TestEnvDrift`) warns on unknown/missing keys. Add a
`validatePushConfig` mirroring `validateTelegramLoginConfig` so "enabled but no
credentials" fails startup instead of silently no-op'ing.

No new dependency: the OAuth2 service-account JWT (RS256) and the access-token
exchange are implemented with `crypto/rsa`, `encoding/json`, `net/http` and
base64url, following the pattern the wallet/login code already uses for
file-backed keys. (If reviewers prefer `golang.org/x/oauth2/google`, that is a
one-line swap — flagged as an open question.)

## 7. FCM sender

`internal/push` (new package) with:

```go
type Encryptor interface {
    // Encrypt returns the TLS-value `p` string for a client token.
    Encrypt(secret []byte, payload []byte) (string, error)
}
type Sender interface {
    Send(ctx context.Context, device domain.PushToken, payload []byte) error
}
```

- **FCM HTTP v1**: `POST https://fcm.googleapis.com/v1/projects/{project}/messages:send`
  with an OAuth2 bearer token; body targets the device token with a `data`
  payload carrying the encrypted `p` and the plaintext routing hints the
  clients read (`loc_key`, `badge`, `custom`, etc.). `data` (not `notification`)
  so the client decrypts and renders its own notification.
- **Encrypted `p` payload** must match the official clients:
  `auth_key_id = SHA1(secret)[0:8]`, random 16-byte message key, AES-IGE over the
  JSON payload, truncated HMAC-SHA256 tag. This is the highest-risk part of the
  whole feature and gets its own spike with **golden vectors captured from the
  two client implementations** before any code lands; do not trust this
  summary, verify against the pinned client sources.
- **Invalidation**: FCM `UNREGISTERED` / `INVALID_ARGUMENT` responses mark the
  token invalidated (or delete it) so it is not retried forever.

## 8. Offline trigger and coalescing

In `OutboxDispatcher`:

- Only `UpdateEventNewMessage` (and later `UpdateEventChannelState` for channel
  posts) are push candidates; `read`, `typing`, `presence` and `noop` are not.
- Respect notify settings: silent for muted peers (`mute_until` in the future)
  and for `no_muted` device rows.
- Trigger exactly when `pushOutboxUpdate` returns `sent == 0` and no error
  (`OnlineUserProvider.IsUserOnline` gives the same answer up front if a
  standalone dispatcher is preferred).
- Coalesce per user with a `TELESRV_PUSH_DEBOUNCE` window so a burst of outbox
  rows produces one push, and cap concurrency with a worker pool so a large
  fan-out cannot starve the outbox.
- The trigger is fire-and-forget from the dispatcher's point of view: a push
  failure never blocks, fails, or retries the update row (delivery correctness
  stays with the durable outbox + `getDifference`).

## 9. APNs (deferred design)

Same `Sender`/`Encryptor` split. The differences: token-based auth (`.p8` key +
key id + team id), `api.push.apple.com` HTTP/2, an `apns-topic`, and a
`mutable-content` notification so the client's notification service extension
can decrypt the same `p`. Requires an Apple key and an iOS rebuild; treated as a
separate follow-up change after FCM is validated.

## 10. Security and privacy

- The service-account JSON is file-backed and never echoed or logged.
- Push bodies carry no plaintext message text server-side; the client decrypts.
- Tokens are per-account credentials: `last_error`/status is logged, never the
  token value; the admin panel only ever shows counts.
- `docs/telegram-differences.md` push row is updated to describe the new
  opt-in behaviour (and that a default build still sends nothing).

## 11. Testing

- Store: memory + postgres (`*_integration_test.go`, skips without the DSN).
- RPC: `registerDevice`/`unregisterDevice` upsert/delete/off-path tests against
  the in-memory store; `deps_validation_test.go`.
- FCM sender: `httptest` server for the token endpoint and the send endpoint;
  token-refresh reuse; `UNREGISTERED` invalidation.
- Encryptor: golden vectors from the clients (the gate for merging §7).
- Outbox: a fake `Sender` proves "push only when `sent == 0`", debounce,
  mute/`no_muted`, and that a disabled sender adds no work.
- Config: parse/validate tests alongside `config_test.go`.

## 12. Rollout

The feature is validated **whole in a fork against a real production
environment** first. Only after it is confirmed there does it become **one PR to
`main`, squashed into maintainable logical commits** — not a series of feature
PRs. The development order below is therefore an internal implementation
staging, not a merge plan:

1. **Storage slice (no behaviour change).** Domain, store interfaces,
   migration, memory/postgres backends, `rpc.Deps` wiring, RPC handlers that
   persist when a store is present but still send nothing, tests. Default
   deployments see no change.
2. **Sender slice behind the flag.** `internal/push`, FCM sender, outbox
   trigger, cipher with golden vectors, config block, docs, `.env.example`,
   `docs/configuration.en.md` §9, `docs/telegram-differences.md`.
3. **Fork/production validation.** Run the built image with
   `TELESRV_PUSH_FCM_ENABLED=true` against a real deployment and a self-built
   client; confirm tokens persist, pushes arrive when offline, and online
   delivery is unchanged.
4. **Single squash to `main`.** Rebase the fork work into a small set of
   logical commits (storage / sender / config+docs) and open one PR.

## 13. Open questions

1. Does the maintainer want the sender **in-tree** (proposed) or as an optional
   sidecar/plugin consuming a webhook like `otpdelivery`? The store + RPC half
   is useful either way.
2. `golang.org/x/oauth2/google` vs. stdlib JWT — reviewer preference.
3. Exact payload field set per client/`token_type`; confirm against the pinned
   Android/iOS sources during the §7 spike.
4. Whether to gate the FCM sender behind an admin-panel toggle in addition to
   the env flag (out of scope unless asked).
