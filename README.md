# nd-simple-rpc

Shows what each user on a [Navidrome](https://www.navidrome.org) server is playing as a Discord "Listening to..." status. It posts from the server, so it works whether or not the user is at a computer or has Discord open.

Pause, stop and expiry clear the status. Cover art is the library's own. Each user connects, pauses and disconnects their own Discord account from a web page.

## How it works

Two parts, shipped in one container image:

- **Plugin** (`nd-simple-rpc.ndp`, WASM, runs inside Navidrome). Receives playback reports, looks up the full title (subtitle included) and a signed cover URL, and forwards them to the companion. It holds no state and no Discord knowledge. Built from `plugin/` with Go's `wasip1` target (TinyGo is used instead if installed).
- **Companion** (`nd-simple-rpc`, Go). Owns the sign-in and connect pages, the OAuth tokens (encrypted in SQLite), and the Discord calls. Its public port serves the web pages; its internal port takes plugin events and must not be exposed.

It uses a real OAuth grant against Discord's headless-session API, not user tokens.

## Discord app setup

Every deployer brings their own Discord application.

1. Create an application in the [Developer Portal](https://discord.com/developers/applications).
2. OAuth2 > Redirects: add `${PUBLIC_URL}/callback` (for example `https://presence.example.com/callback`).
3. Submit the Social SDK "Getting Started" form for the app. Without it, authorization fails with `invalid_scope`.
4. Leave "Public Client" off. The companion uses the client secret.
5. Copy the Client ID and Client Secret.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `DISCORD_CLIENT_ID` | yes | Discord application client ID |
| `DISCORD_CLIENT_SECRET` | yes | Discord application client secret |
| `PUBLIC_URL` | yes | Public base URL of the companion, no trailing slash. The redirect URI is `${PUBLIC_URL}/callback` |
| `NAVIDROME_URL` | yes | Navidrome base URL as the companion reaches it (used to check logins) |
| `SHARED_SECRET` | yes | Bearer secret the plugin sends to the internal endpoint |
| `ENCRYPTION_KEY` | yes | 32 bytes, base64. Encrypts stored tokens (AES-GCM) |
| `SESSION_KEY` | yes | 32 bytes, base64. Signs session cookies |
| `DB_PATH` | yes | SQLite file path, on a persistent volume |
| `PUBLIC_ADDR` | no | Public listen address. Default `:8080` |
| `INTERNAL_ADDR` | no | Internal listen address. Default `:8081` |

Generate keys with `openssl rand -base64 32`.

## Deploy

See [`examples/docker-compose.yml`](examples/docker-compose.yml). The shape:

1. `nd-simple-rpc-plugin` is a one-shot container that runs `nd-simple-rpc install-plugin /data/plugins` against Navidrome's data volume. It writes atomically and does nothing if the file is already identical.
2. `navidrome` waits for it (`service_completed_successfully`) and runs with `ND_PLUGINS_AUTORELOAD=true`, so a new plugin version loads without a restart.
3. `nd-simple-rpc` is the companion. Put a reverse proxy in front of the public port (`8080`) only.

`install-plugin` takes `--src` to install a plugin file other than the bundled `/plugin/nd-simple-rpc.ndp`. The `.ndp` is also attached to each GitHub release for manual installs.

### Service name

The companion's Compose service (or Kubernetes Service) must be named `nd-simple-rpc`. The plugin's manifest allows outbound HTTP only to the host `nd-simple-rpc`, `nd-simple-rpc.*` and the private ranges `10.0.0.0/8`, `172.16.0.0/12` and `192.168.0.0/16`. Any other name is blocked by Navidrome.

## One-time Navidrome step

As an admin, in Navidrome's plugin settings:

1. Enable the `nd-simple-rpc` plugin.
2. Set `companionUrl` to the internal address, for example `http://nd-simple-rpc:8081`.
3. Set `sharedSecret` to the same value as the companion's `SHARED_SECRET`.
4. Grant the plugin access to all users (it needs this to look up track details as each user).

Then each user opens the companion's public URL, signs in with their Navidrome account, and clicks Connect Discord. Discord's consent screen lists friends-list permissions the service does not use; that is the cost of the only scope that allows presence.

## Limitations

- **Legacy Subsonic clients.** Navidrome turns the legacy `scrobble?submission=false` call into a "playing" report only and never sends "paused". For those clients the status stays up until Navidrome expires the session at the track's expected end. Clients that use `reportPlayback`, Jellyfin-style sessions or the web UI send pause and stop correctly.
- **Multiple devices.** One status per user; the most recent event wins.
- **Navidrome target.** Built against Navidrome 0.64.x.

## Terms of service

Headless sessions are not part of Discord's official documentation. Discord's Developer Terms and Social SDK Terms prohibit use "not in accordance with the Documentation", so this use may fall outside them, and Discord may change or remove the endpoint or act on accounts or apps at its discretion. This is not a self-bot: users grant access through OAuth and no user token is handled. You run it at your own risk.

## Development

```
make test                 # companion (race) and plugin tests
make -C plugin package    # builds plugin/nd-simple-rpc.ndp
docker build -t nd-simple-rpc:dev .
```

Releases: pushing a `v*` tag builds and pushes `ghcr.io/laforcem/nd-simple-rpc:<tag>` and `:latest` and attaches the `.ndp` to the GitHub release.
