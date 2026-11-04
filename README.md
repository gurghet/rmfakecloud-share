# rmfakecloud-share

reMarkable tablets have a toolbar button, **Convert and Share**, that normally
needs a paid reMarkable Connect subscription. If you host your own cloud with
[rmfakecloud](https://github.com/ddvk/rmfakecloud) the button is missing.

`rmfakecloud-share` is a small sidecar service that brings it back. You tap the
button, the tablet shows a QR code, and scanning it opens a web page with your
handwriting transcribed to Markdown by Claude, next to the original pages.

> **Privacy: page images are sent to Anthropic.** When `ANTHROPIC_API_KEY` is
> set, every page you share is uploaded to the Anthropic Messages API for
> transcription. Do not share pages you are not willing to send to a third
> party. Without a key, nothing leaves your server and shares only show the page
> images. See [Privacy](#privacy).

## How it works

```
tablet (xochitl)        reverse proxy          rmshare             Anthropic API
     |  POST /share/v1/link  |                    |                      |
     |---------------------->|------------------->| verify JWT, store    |
     |                       |                    | pages, answer now    |
     |<-- {"link": ".../share/<id>/"} ------------|                      |
     |  shows QR code        |                    |--- pages (async) --->|
                                                  |<-- Markdown ---------|
phone scans QR --> GET /share/<id>/ --> "Converting..." then the note
```

The reverse proxy sends only `POST /share/v1/link` to `rmshare`. Everything
else keeps going to rmfakecloud. The tablet gets its answer immediately, and
the conversion runs in a single background worker, so a Raspberry Pi is enough.

Protocol details are in [docs/protocol.md](docs/protocol.md).

## Requirements

- A working rmfakecloud (tested with 0.0.31) and a tablet running `xochitl`
  3.29.1.0 that already syncs with it.
- A reverse proxy in front of both (Cloudflare Tunnel, nginx, Caddy, ...).
- Go 1.25 or newer to build from source, or Docker.
- Optional: an Anthropic API key for the transcription.

## Install

Build from source:

```sh
git clone https://github.com/gurghet/rmfakecloud-share
cd rmfakecloud-share
go build -o rmshare ./cmd/rmshare
# Raspberry Pi (64-bit): CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o rmshare ./cmd/rmshare
```

Docker (the image is static, runs as a non-root user, and keeps state in `/data`):

```sh
docker build -t rmshare .
docker run -d --name rmshare -p 127.0.0.1:3004:3004 -v rmshare-data:/data \
  -e RMSHARE_PUBLIC_URL=https://notes.example.com/share \
  -e JWT_SECRET_KEY=... -e ANTHROPIC_API_KEY=... rmshare
```

systemd: see [contrib/rmshare.service](contrib/rmshare.service). Put the
variables in `/etc/rmshare.env` (mode 600).

## Configuration

All configuration is through environment variables. The service refuses to start
if a required one is missing.

| Variable | Default | Meaning |
| --- | --- | --- |
| `RMSHARE_LISTEN` | `127.0.0.1:3004` | Address to listen on. |
| `RMSHARE_PUBLIC_URL` | required | Base URL of the share pages as the phone sees it, ending in `/share`, for example `https://notes.example.com/share`. Links returned to the tablet are `<RMSHARE_PUBLIC_URL>/<id>/`. |
| `RMSHARE_DATA_DIR` | required | Where shares are stored (`shares/<id>/`). |
| `JWT_SECRET_KEY` | required | The same value rmfakecloud uses. |
| `RMSHARE_JWT_KEY_MODE` | `rmfakecloud` | `rmfakecloud` derives the HMAC key with PBKDF2 like rmfakecloud v0.0.31. `raw` uses the secret as-is, for other servers. |
| `ANTHROPIC_API_KEY` | empty | Enables conversion. Empty means shares work but show "conversion disabled". |
| `RMSHARE_MODEL` | `claude-opus-5-5` | Model used for transcription. |
| `RMSHARE_MAX_BODY_BYTES` | `67108864` (64 MiB) | Largest accepted upload. Larger requests get 413. |
| `RMSHARE_MAX_PAGES` | `50` | Most pages per share. More gets 400. |

If you start the service without an API key and add one later, shares that were
still pending are converted on the next start.

## rmfakecloud setup

1. In `<DATADIR>/users/<user>/.userprofile` add the scope that unlocks the button:

   ```yaml
   additionalscopes:
     - share-url
   ```

2. Make the tablet fetch a new token, for example by restarting the app on the
   tablet: `systemctl restart xochitl`.
3. Route `POST /share/v1/link` to `rmshare` (next section). The tablet sends it to
   the same host it uses for rmfakecloud.

The `JWT_SECRET_KEY` given to `rmshare` must equal the one rmfakecloud runs with.

## Reverse proxy routing

Only `POST /share/v1/link` must reach `rmshare` on the host the tablet uses. The
page URLs (`/share/<id>/...`) are opened by your phone, see the note after the
examples.

### Cloudflare Tunnel

Put the path rule **before** the hostname catch-all, since rules match top to
bottom:

```yaml
ingress:
  - hostname: notes.example.com
    path: ^/share/v1/link$
    service: http://127.0.0.1:3004
  - hostname: notes.example.com
    service: http://127.0.0.1:3000   # rmfakecloud
  - service: http_status:404
```

### nginx

```nginx
server {
    server_name notes.example.com;
    # ... TLS ...
    client_max_body_size 64m;

    location = /share/v1/link {
        proxy_pass http://127.0.0.1:3004;
        proxy_request_buffering off;
    }
    location /share/ {            # share pages, if served on this host too
        proxy_pass http://127.0.0.1:3004;
    }
    location / {
        proxy_pass http://127.0.0.1:3000;   # rmfakecloud
    }
}
```

### Caddy

```caddyfile
notes.example.com {
    @share path /share/v1/link /share/*
    handle @share {
        reverse_proxy 127.0.0.1:3004
    }
    handle {
        reverse_proxy 127.0.0.1:3000   # rmfakecloud
    }
}
```

### Which host serves the page?

The QR code carries `RMSHARE_PUBLIC_URL`. It must be reachable **from the phone
that scans it**. It can be a different host from the one the tablet talks to, for
example a LAN-only or VPN-only name. That host needs to route `/share/` to
`rmshare`, and `RMSHARE_PUBLIC_URL` must still end in `/share`.

## Privacy

- With `ANTHROPIC_API_KEY` set, **all pages of every share are sent to the
  Anthropic API.** Review Anthropic's terms and data retention policy for your
  account before using it with sensitive notes.
- Without the key, no page leaves your server.
- Share pages are stored unencrypted under `RMSHARE_DATA_DIR`. There is no
  automatic expiry in v1, so delete `shares/<id>/` directories you no longer need.
- Pages send `Referrer-Policy: no-referrer` and `X-Robots-Tag: noindex`, and load
  no external assets.

## Security model

- **Uploads are authenticated.** `POST /share/v1/link` requires a Bearer JWT, HS256
  only (`none` and other algorithms are rejected), with a valid signature, `exp` and
  `nbf` (30 s skew allowed), and `share-url` in `scopes`. Tokens are never logged.
- **Reading is a capability URL.** `GET /share/<id>/...` is unauthenticated: anyone
  who has the link can read the note. IDs are 128 random bits (base64url) and are
  validated against a strict pattern before touching the filesystem. Treat links like
  passwords, and prefer a LAN or VPN-only host for the page URL if notes are private.
- **Model output is untrusted.** Markdown is rendered without raw HTML passthrough,
  and the pages carry a strict `Content-Security-Policy` (no external sources, script
  and style only with a per-response nonce). A note containing `<script>` is shown
  as inert text or dropped.
- Uploads are limited by size (`RMSHARE_MAX_BODY_BYTES`), page count and PNG magic bytes.

## Limitations

- `MailList` (addresses typed on the tablet) is stored and logged, but no mail is
  sent. This is future work.
- Tested against xochitl 3.29.1.0 and rmfakecloud 0.0.31 only.
- Handwriting transcription can be wrong. The page always shows the originals.
- No expiry or deletion API for old shares.
- If the request is retried by the tablet, each retry creates a new share.

## Credits

The protocol was reverse-engineered from xochitl. Prior art:
[coleleavitt/remarkable-server](https://github.com/coleleavitt/remarkable-server)
serves raw PNGs for the same endpoint. Built to sit next to
[rmfakecloud](https://github.com/ddvk/rmfakecloud) by ddvk and contributors. This
project is not affiliated with reMarkable AS.

## Development

```sh
gofmt -l .          # must print nothing
go vet ./...
go test -race -count=1 ./...
```

## Licence

MIT, see [LICENSE](LICENSE).
