# EutherPunk CLI authentication

## Goal

EutherPunk should feel passwordless on a known computer without copying an
EutherID secret into the CLI. The browser remains the authentication surface;
the CLI receives only a narrowly scoped, revocable EutherPunk credential.

## Device flow

1. The CLI creates a random PKCE verifier and sends only its S256 challenge to
   `POST /api/eutherpunk/auth/device`.
2. The server returns a five-minute device code, a human-readable user code and
   an HTTPS verification URL.
3. The CLI opens that URL. EutherPunk forwards the browser cookie over the
   loopback/server network to EutherOxide `/api/app/status`.
4. EutherOxide must report an authenticated session with
   `eutherIdVerified=true`. A password-created session is deliberately
   insufficient.
5. The page shows the CLI name, account and the exact `eutherpunk:chat` and
   `eutherpunk:media` scopes. The user explicitly approves or denies them. POST
   approval is CSRF- and same-origin-protected.
6. The CLI polls with the opaque device code and its original PKCE verifier.
   One successful exchange consumes the device grant.
7. The server issues a one-hour access token and a rotating 30-day refresh
   token. `/api/eutherpunk/auth/revoke` revokes every token in that family.

## Storage and authorization

- Device, access and refresh secrets are cryptographically random.
- The server JSON state contains only SHA-256 hashes, is created with mode
  `0600`, and is replaced atomically.
- Windows stores client credentials as a Generic Credential in Windows
  Credential Manager, keyed by the EutherPunk API URL.
- The refresh token is rotated on every use. A consumed refresh token cannot be
  reused.
- CLI bearer tokens have only `eutherpunk:chat` and `eutherpunk:media`.
  `eutherpunk:media` permits server-side image and voice jobs but grants no
  local file access. Browser principals may use other routes according to
  their EutherOxide admin role.
- Request user identity comes from the authenticated principal. Public
  `X-User`-style headers are not trusted.

## Public and protected routes

Public:

- web shell and static assets
- `GET /api/eutherpunk/status`
- CLI downloads
- device start, token exchange and refresh bootstrap
- browser approval page (approval still requires EutherID)

Protected:

- model listing and chat
- user/admin prompt APIs
- server settings
- stored conversations
- TTS and image APIs

The public device endpoints reveal no account or session data and are bounded
to 20 simultaneous, five-minute grants. They should still be covered by normal
reverse-proxy request limits and monitoring.

## Deliberate boundaries

This authenticates access to EutherPunk; it does not grant local file, shell or
administrator rights. A separate local workspace permission may grant bounded
file access, including saving a generated PNG under the selected workspace,
but the bearer token itself never does so. An
EutherID browser session proves who approved the client, not that every later
local action is safe. Future tools therefore need their own visible
permissions and per-action approval rules.
