# Megaphone publisher

A tiny self-contained tool to publish a (video) podcast episode to Megaphone.fm.
It uploads the video, creates the episode, waits for processing, sets the ad
cuepoints, and publishes — so you don't have to click through the CMS each week.

There is **nothing to install** to run it: it's a single binary plus one config
file. "Put these two files in a folder and run the app."

## For the person running it

1. Put the `megaphone-publisher` binary and a `megaphone.toml` in the same folder.
2. Fill in `megaphone.toml` (copy `megaphone.example.toml` as a starting point).
3. Run it:
   - macOS: open Terminal in that folder and run `./megaphone-publisher`
   - Windows: double-click `megaphone-publisher.exe` (or run it from a terminal)
4. Answer the questions (title, description, clean title, season/episode,
   type, rating, the video file, length, mid-roll timestamps). It does the rest.
   - Season and episode numbers default to **"latest episode + 1"** (read live
     from Megaphone) — just press Enter to accept.
   - The description accepts `@path/to/file.html` to load a long description
     from a file instead of typing it in.

Try a `--dry-run` first — it shows exactly what it *would* do (including the
cuepoints) without uploading anything or touching Megaphone.

## What it does, step by step

1. **Uploads** the video to your S3 bucket and makes a temporary signed link.
   (Megaphone fetches media *by URL* — it does not accept a pushed file — so the
   file is staged in S3 first.)
2. **Creates the episode as a draft** — media supplied as that URL, no pubdate.
3. **Waits** for Megaphone to finish processing the video (polls every 30s).
4. **Adds cuepoints**, then **publishes** (sets the pubdate).

### Cuepoint rules (configurable in `megaphone.toml`)

- **Post-roll:** always, `postroll_ad_count` slots.
- **Pre-roll:** only if the episode is longer than `min_seconds_for_ads`,
  `preroll_ad_count` slots.
- **Mid-roll:** only if longer than `min_seconds_for_ads`, one break per
  timestamp you enter.

"3 slots" is modelled as a single break with `adCount: 3`. If you actually want
three *separate* breaks, say so and it's a one-line change.

## Building (for whoever distributes it)

```sh
go build -o megaphone-publisher .

# Cross-compile from one machine for everyone:
GOOS=darwin  GOARCH=arm64 go build -o dist/megaphone-publisher-mac-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o dist/megaphone-publisher-mac-intel .
GOOS=windows GOARCH=amd64 go build -o dist/megaphone-publisher.exe .
```

## Notes / assumptions (worth verifying on the first real run)

These come from the Megaphone API blueprint and need a live token + bucket to
confirm end-to-end:

- **Podcast-level setup is required once.** For a video episode the *podcast*
  must have `ownerName`, `ownerEmail`, and `imageFile` set, or Megaphone
  publishes the upload as **audio**. Cuepoints must also be **enabled on the
  podcast** or the cuepoint calls won't take effect.
- **Status / draft model.** A `pubdate` is **required** even for drafts (the API
  rejects a blank one with 400). The episode's `draft` boolean is what keeps it
  out of the live feed. So the tool creates every episode with `draft: true` + a
  placeholder pubdate, then publishing flips `draft: false` and sets the pubdate.
  `-no-publish` simply stops after the draft is created.
- **Post-roll `startTime`.** The cuepoint schema documents `startTime` in seconds
  from 0; for post-roll this tool sends the episode duration (EOF). If Megaphone
  expects a different convention for post-roll, that's the line to adjust in
  `buildCuepoints`.
- **Rate limit** is 60 requests/minute; the processing poll runs every 30s to
  stay well under it.

## Not done yet

- YouTube publishing (you mentioned it's for later).

## Install

Download a prebuilt binary from the [Releases](https://github.com/rahoulb/megaphone-publisher/releases)
page, or build from source:

```sh
go install github.com/rahoulb/megaphone-publisher@latest
# or
git clone https://github.com/rahoulb/megaphone-publisher && cd megaphone-publisher && go build .
```

## License

Copyright (C) 2026 Rahoul Baruah. Licensed under the
[GNU Affero General Public License v3.0 or later](LICENSE) (AGPL-3.0-or-later).
