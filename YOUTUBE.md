# YouTube publishing — design note

**Status: planned, not built.** This captures the agreed design and the YouTube
specifics so it's ready to pick up. Nothing here is implemented yet.

## The idea

Publish each episode to **both** Megaphone and YouTube from the same run,
reusing the single slow upload as much as possible.

- Megaphone *pulls* media from a URL (our presigned S3 link) — already how the
  tool works.
- **YouTube cannot pull.** The Data API `videos.insert` is **push-only**
  (resumable upload); there is no "fetch from URL" option. So the bytes have to
  be pushed to YouTube directly from the machine running the tool.

We chose **option 2 (keep it a single self-contained binary)** over a cloud
worker. That means the file leaves the machine **twice** per episode: once to S3
(for Megaphone) and once to YouTube. Unavoidable without a cloud component, which
we decided is overkill for a small utility.

### Serial by default, `--fast` for parallel

- **Default (serial):** upload to S3 → kick off Megaphone's import → *then*
  start the YouTube upload. On a thin uplink this is best: Megaphone is already
  processing while YouTube uploads, so the two slow things still overlap without
  saturating the connection.
- **`--fast`:** run the S3 and YouTube uploads concurrently. For fat pipes with
  headroom.

We deliberately are **not** auto-detecting connection speed: a synthetic
speedtest is noisy, adds dead time, and a wrong guess is worse than no guess. The
user knows their connection. (If we ever want auto-tuning, the sound version is
to measure the *real* S3 upload throughput in-flight — we already track upload
bytes for the progress bar — and decide whether to fire YouTube alongside it
after the first ~30s/~100MB. v2 nicety, not now.)

### Draft → review → publish

Mirror the Megaphone draft model: upload as **`unlisted`** (or `private`), leave
it for review, then flip to `public` with `videos.update`. With `-no-publish`
the YouTube video stays unlisted, same as the Megaphone draft.

### Not possible via the API (don't promise these)

- **Monetisation / ad settings** — managed in YouTube Studio (or the Content
  Owner API for partners), not the public Data API.
- **Mid-roll ad break placement** — not in the Data API. So the cuepoint logic
  does **not** carry over to YouTube; ad breaks are YouTube's to place.

## Metadata YouTube needs (`videos.insert`)

`videos.insert` takes `part=snippet,status` (+ the media). Fields:

### snippet

| Field | Required | Source | Notes |
|---|---|---|---|
| `title` | **yes** | reuse the episode title | ≤100 chars, no `< >` |
| `description` | no | reuse the episode description | ≤5000 bytes, no `< >` |
| `tags[]` | no | config default + per-episode | ≤500 chars total |
| `categoryId` | **yes** | config (default **"17" = Sports**) | region-dependent; confirm via `videoCategories.list` |
| `defaultLanguage` | no | config (default `en`) | language of title/description |
| `defaultAudioLanguage` | no | config (default `en`) | spoken language |

### status

| Field | Required | Default we'd send | Notes |
|---|---|---|---|
| `privacyStatus` | no | `unlisted` on upload → `public` to go live | `private`/`unlisted`/`public` |
| `selfDeclaredMadeForKids` | **effectively yes** | **`false`** ("Made for kids → NO") | COPPA declaration; YouTube wants an explicit audience |
| `containsSyntheticMedia` | no | `false` | altered/synthetic (AI) content disclosure |
| `license` | no | `youtube` | or `creativeCommon` |
| `embeddable` | no | `true` | |
| `publicStatsViewable` | no | leave default | stats on watch page |
| `publishAt` | no | unset | scheduled go-live; **requires `privacyStatus: private`** |

Insert call parameter `notifySubscribers` (default true) — worth a config toggle.

## Subtitles / captions (the `.srt`)

A separate call **after** the video exists — `captions.insert`:

- **snippet:** `videoId` (the new video), `language` (e.g. `en`), `name` (track
  label, e.g. "English"), optional `isDraft`.
- **media:** the caption file, up to 100MB. The API accepts `application/octet-stream`
  (SubRip `.srt` works in practice; YouTube parses it).
- **Scope:** needs `youtube.force-ssl` (broader than upload). **Quota: 400 units.**
- In the tool: an **optional `.srt` path** prompt; if given, upload it once the
  video insert returns an id.

## Optional extras (later)

- **Thumbnail** — `thumbnails.set` (channel must be verified).
- **Playlist** — `playlistItems.insert` to drop the episode into a season /
  "Group of Death" playlist.

## OAuth & quota — the real friction (flag before building)

YouTube is **not** a paste-a-token affair like Megaphone:

- **OAuth 2.0** with scopes `youtube.upload` **and** `youtube.force-ssl` (the
  latter for captions). Both are *sensitive* scopes. Per-channel: each person
  uploading authorises their own channel; the OAuth client lives in a Google
  Cloud project (Baz's).
- **Refresh-token expiry gotcha:** while the OAuth app is in **"Testing"**
  publishing status, refresh tokens **expire after 7 days** → weekly re-auth.
  Long-lived tokens require moving the app to **"In production"**, which for
  sensitive scopes can trigger Google **app verification**. This materially
  affects the "hand it to your brother" model and is the main thing to resolve
  before building.
- **Quota:** default 10,000 units/day. `videos.insert` ≈ **1600**, `captions.insert`
  = **400**, a privacy-flip `videos.update` ≈ 50. So ~5 full publishes/day on the
  default quota — fine for a weekly show, but it's a ceiling to be aware of.

## Proposed config additions (sketch)

```toml
[youtube]
enabled = true
# OAuth client (from the Google Cloud project) + where the refresh token is cached
client_secret_file = "youtube_client_secret.json"
token_cache_file   = "youtube_token.json"
category_id = "17"                 # Sports
default_language = "en"
default_audio_language = "en"
privacy_on_upload = "unlisted"     # flip to public on publish
made_for_kids = false              # "Made for kids → NO"
contains_synthetic_media = false
license = "youtube"
notify_subscribers = true
tags = ["Nottingham Forest", "1865", "World Cup", "football"]
playlist_id = ""                   # optional
```

Per-episode prompts to add: **subtitles `.srt` path (optional)**, and probably a
**tags** override. Title/description are reused from what's already entered.

## Build order when greenlit

1. Google Cloud project + OAuth client; resolve the Testing-vs-production /
   verification question (it gates the whole "others can use it" story).
2. OAuth device/loopback flow in the binary; cache + refresh the token.
3. Resumable `videos.insert` (unlisted) running serial-by-default / `--fast`.
4. `captions.insert` for the optional `.srt`.
5. `videos.update` to flip `unlisted → public` at publish time (skipped under
   `-no-publish`).
6. Optional: playlist add, custom thumbnail.
