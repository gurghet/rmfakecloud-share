# Wire protocol

This documents what the reMarkable app (`xochitl` 3.29.1.0) sends for the
"Convert and Share" toolbar button and what it accepts back. It was derived
from static analysis of the binary and live captures. All values below are
synthetic examples.

## 1. Enabling the button

`xochitl` shows the button only if the user JWT's `scopes` claim contains the
token `share-url`. `scopes` is a single space-separated string:

```
"intgr screenshare docedit hwcmail:-1 hwc mail:-1 sync:tortoise share-url"
```

With rmfakecloud, add the scope per user in `<DATADIR>/users/<user>/.userprofile`:

```yaml
additionalscopes:
  - share-url
```

The tablet reads the scopes the next time it fetches a token, for example after
`systemctl restart xochitl`.

## 2. Request

```
POST /share/v1/link HTTP/1.1
Host: <tectonic>.tectonic.remarkable.com
Authorization: Bearer <user JWT>
User-Agent: xochitl/3.29.1.0 (codex 6.0.112)
Content-Type: multipart/form-data; boundary="boundary_.oOo._f2NyZPUjFTnDwQ9PIDSHS7MfwPG8ZGpD"
Content-Length: 167453
```

The tablet's DNS or proxy setup for rmfakecloud already sends the host to the
user's own server. Notes on the encoding:

- The boundary parameter is **quoted** and contains `.` and `_`. Parse it with a
  real media-type parser, not a regular expression.
- Part header names are **lowercase** (`content-disposition`, not
  `Content-Disposition`). Header names are case-insensitive, but hand-written
  parsers sometimes forget.
- Parts, in order: one `metadata` part, then one `page` part per page.

```
--boundary_.oOo._f2NyZPUjFTnDwQ9PIDSHS7MfwPG8ZGpD
content-disposition: form-data; name="metadata"
content-type: application/json

{
    "DocID": "d0c1d0c1-0000-4000-8000-000000000001",
    "MailList": [],
    "Pages": [
        "9a9e0001-0000-4000-8000-000000000001"
    ]
}
--boundary_.oOo._f2NyZPUjFTnDwQ9PIDSHS7MfwPG8ZGpD
content-length: 166912
content-disposition: form-data; name="page"; filename="9a9e0001-0000-4000-8000-000000000001.png"
content-type: image/png

<PNG bytes>
--boundary_.oOo._f2NyZPUjFTnDwQ9PIDSHS7MfwPG8ZGpD--
```

The metadata part has no `content-length`. Page parts do, and each filename is
`<page-uuid>.png` where the UUID is one of the entries of `Pages`, in the same
order. `MailList` is a list of addresses entered on the tablet. This
implementation stores and logs it but does not send any mail.

## 3. Response

HTTP 200 with:

```
Content-Type: application/json

{"link": "https://notes.example.com/share/<id>/"}
```

The tablet renders the URL as a QR code. When scanned, the phone opens the URL
with extra query parameters appended:

```
?rm_utm_source=shared_webpage&rm_utm_campaign=create_open_link&rm_utm_medium=qr_code
```

The page handler must ignore the query string.

On HTTP 401 the tablet refreshes its token and retries about five times, then
shows "unable to send". Answer promptly: the link is returned as soon as the
upload is stored, and the conversion runs afterwards.

## 4. Token verification

rmfakecloud (v0.0.31) signs user tokens with HS256 and the header
`{"alg":"HS256","kid":"1","typ":"JWT"}`. The HMAC key is **not** the
`JWT_SECRET_KEY` value itself. It is derived as

```
key = PBKDF2-HMAC-SHA256(password = JWT_SECRET_KEY, salt = "todo some salt", iterations = 10000, length = 32)
```

(rmfakecloud `internal/config/config.go`). Verifying with the raw secret makes
every request fail with 401. Claims include `sub` (user id), `scopes` (string),
`exp`, `nbf`, `iat`, `device-id`, `level` and `iss` (`"rM WebApp"`).

`rmshare` accepts only HS256, checks the signature in constant time, checks
`exp` and `nbf` with 30 seconds of clock skew, and requires `share-url` among
the `scopes`.
