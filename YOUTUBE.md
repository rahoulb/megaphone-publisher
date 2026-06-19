# YouTube publishing — design note

**Status: ON HOLD** on branch `feature/youtube-publishing` (not merged to
develop). YouTube is being published **manually** for now.

Done: OAuth loopback sign-in (`-youtube-login`), token caching, playlist listing
(`-youtube-check`), and the (unwired) insert/wait/publish/playlist methods.

**Blocker — Brand Account channel selection.** The 1865 channel is a Brand
Account; the operator (rahoul.baruah@gmail.com) is a *manager* and can switch
into it on youtube.com. But during OAuth, Google shows only the personal Google
account at "Choose an account" and **never presents the YouTube "Choose a
channel" step**, so the token resolves to the personal channel and
`-youtube-check` lists personal playlists. Tried: revoking the app's prior grant
at myaccount.google.com/connections, deleting the cached token, and
`prompt=select_account consent` — the channel chooser still doesn't appear.

Plan B to try when resumed (any of):
- Sign in with / get the credentials of the Google account that *owns* (not just
  manages) the 1865 Brand Account.
- Have an Owner of the Brand Account confirm the operator's role is **Owner**
  (some brand channel selectors only surface for owners, not managers).
- Investigate whether a dedicated Google account should be made a Brand Account
  owner specifically for API publishing.

Still to do after auth is unblocked: the upload → wait-for-scan → publish
orchestration and the `--fast`/serial wiring.

## Setup: create the Google OAuth client (do this once)

1. Go to <https://console.cloud.google.com/> and create (or pick) a project.
2. **APIs & Services → Library →** enable **"YouTube Data API v3"**.
3. **APIs & Services → OAuth consent screen:** User type **External**; fill in
   app name + your email. Add yourself (and anyone else who'll publish) as
   **Test users**. (While in "Testing", refresh tokens expire after 7 days —
   fine to start; publish/verify the app later to make them long-lived.)
4. **APIs & Services → Credentials → Create Credentials → OAuth client ID →**
   Application type **Desktop app**. Download the JSON.
5. Save that JSON next to the binary as `youtube_client_secret.json` (matches
   `client_secret_file` in the config), set `[youtube] enabled = true`, then run
   `./megaphone-publisher -youtube-login` once, and `-youtube-check` to confirm.

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

No auto speed-detection: a synthetic speedtest is noisy and a wrong guess is
worse than no guess. (Possible v2: measure the real S3 throughput in-flight and
decide. Not now.)

## Upload → wait for YouTube's scan → publish

This mirrors the Megaphone draft→process→publish flow, and it's the safe order:
**never set a video `public` while YouTube is still processing it.** If their
automated checks flag something on an already-public video, that's the
worst-case (strike/blacklist) we want to avoid. So:

1. **Upload as `private`** (not even unlisted — fully hidden until cleared).
2. **Poll** `videos.list` with `part=status,processingDetails` until YouTube has
   finished:
   - ready when `processingDetails.processingStatus == "succeeded"` **and**
     `status.uploadStatus == "processed"`.
   - **abort/publish-block** if `status.uploadStatus == "rejected"` — report
     `status.rejectionReason` (e.g. `copyright`, `duplicate`, `inappropriate`)
     and do **not** go public.
3. **Publish:** `videos.update` sets `privacyStatus = public`.

Under `-no-publish`, stop after step 2 and leave it **private** for manual review
in Studio — same as the Megaphone draft.

**Honest caveat:** "processed & not rejected" is the best *programmatic* signal
that YouTube has finished its initial pass; it is not a guaranteed human
all-clear, and Content ID claims can still appear later. But it removes the
"public while still processing" window, which is the actual risk you raised.

## "Categories" = playlists

What the account calls categories (e.g. **"1865 Group of Death"**) are
**playlists** in API terms. Flow:

- On run, `playlists.list?mine=true&part=snippet` → list the channel's playlists
  by title.
- Let the user **tick one or more** that apply (config can hold the usual
  defaults, e.g. always add to "1865 Group of Death").
- After the video is created, `playlistItems.insert` once per selected playlist
  (`snippet.playlistId` + `snippet.resourceId = {kind: youtube#video, videoId}`).

This is separate from the API's required `categoryId` (the fixed YouTube list) —
we set that quietly from config (default **"17" = Sports**).

## Metadata YouTube needs (`videos.insert`, `part=snippet,status`)

### snippet

| Field | Required | Source | Notes |
|---|---|---|---|
| `title` | **yes** | reuse the episode title | ≤100 chars, no `< >` |
| `description` | no | reuse the episode description | ≤5000 bytes, no `< >` |
| `tags[]` | no | config default + per-episode | ≤500 chars total |
| `categoryId` | **yes** | config (default **"17" = Sports**) | the fixed YouTube list, *not* the playlists above |
| `defaultLanguage` | no | config (default `en`) | language of title/description |
| `defaultAudioLanguage` | no | config (default `en`) | spoken language |

### status

| Field | Required | Value we'd send | Notes |
|---|---|---|---|
| `privacyStatus` | no | `private` on upload → `public` after scan | holding state is private |
| `selfDeclaredMadeForKids` | **effectively yes** | **`false`** ("Made for kids → NO") | COPPA declaration |
| `containsSyntheticMedia` | no | `false` | altered/synthetic (AI) disclosure |
| `license` | no | `youtube` | or `creativeCommon` |
| `embeddable` | no | `true` | |
| `publishAt` | no | unset | scheduled go-live; needs `privacyStatus: private` (could power a "schedule" mode later) |

Insert parameter `notifySubscribers` (default true) — worth a config toggle.

## Subtitles — deferred

`.srt` upload is **out of scope for now** (Baz will add captions manually in
Studio when needed). If we revisit: it's a separate `captions.insert` call after
the video exists (needs the `youtube.force-ssl` scope, 400 quota units, accepts
SubRip). Noted here so it's not forgotten.

## OAuth — browser sign-in

The "click a link, sign in, done" experience is the standard **installed-app
loopback flow**, and it's a good fit:

1. The tool starts a tiny local web server on `127.0.0.1:<port>`.
2. It opens the browser to Google's consent screen.
3. You sign in and approve; Google redirects back to `127.0.0.1`, the tool
   catches the code and exchanges it for tokens, then caches the refresh token.

Subsequent runs use the cached refresh token silently — no browser.

**Scopes:** more than just upload, because we also flip privacy and add to
playlists:
- `youtube.upload` (insert), **plus** a read/write scope —
  `https://www.googleapis.com/auth/youtube` — for `videos.update`,
  `playlists.list`, and `playlistItems.insert`. (`youtube.force-ssl` would also
  cover all of these, and captions later, in one scope.)

**Refresh-token caveat (still applies):** while the OAuth app is in Google
"Testing" status, refresh tokens **expire after 7 days** → weekly re-sign-in.
Long-lived tokens need the app moved to "In production", which for these
sensitive scopes can trigger Google app verification. Fine for "just Baz" in
testing; the thing to resolve before sharing it wider.

## Quota

Default 10,000 units/day. `videos.insert` ≈ **1600**, each `playlistItems.insert`
≈ **50**, the publish `videos.update` ≈ **50**, status polls ≈ **1** each. Easily
a handful of full publishes/day — fine for a weekly show.

## Not possible via the Data API (don't promise these)

- **Monetisation / ad settings** — Studio only.
- **Mid-roll ad break placement** — not in the API; cuepoint logic does **not**
  carry over to YouTube.

## Proposed config additions (sketch)

```toml
[youtube]
enabled = true
client_secret_file = "youtube_client_secret.json"  # OAuth client from Google Cloud project
token_cache_file   = "youtube_token.json"          # cached refresh token
category_id = "17"                 # Sports (the fixed YouTube list)
default_language = "en"
default_audio_language = "en"
privacy_after_scan = "public"      # holding state is always private until cleared
made_for_kids = false              # "Made for kids → NO"
contains_synthetic_media = false
license = "youtube"
notify_subscribers = true
tags = ["Nottingham Forest", "1865", "World Cup", "football"]
default_playlists = ["1865 Group of Death"]   # pre-ticked; user can add/remove at the prompt
```

Per-episode prompts to add: **which playlist(s)** apply (pre-ticked from
`default_playlists`), and optionally a **tags** override. Title/description are
reused from what's already entered for Megaphone.

## Build order when greenlit

1. Google Cloud project + OAuth client (Desktop app type); decide Testing vs
   production/verification (gates the "others can use it" story).
2. Loopback OAuth flow in the binary; cache + refresh the token.
3. `playlists.list` → present playlists, multi-select.
4. Resumable `videos.insert` as **private**, serial-by-default / `--fast`.
5. Poll `processingDetails`/`status` until processed & not rejected (handle
   `rejected` → stop, report reason).
6. `playlistItems.insert` for each chosen playlist.
7. `videos.update` to flip `private → public` (skipped under `-no-publish`).
8. Later/optional: subtitles, custom thumbnail, scheduled `publishAt`.
