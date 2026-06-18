// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Rahoul Baruah

// megaphone-publisher: a single self-contained tool to publish a (video)
// podcast episode to Megaphone.fm — upload the file, create the episode, wait
// for processing, set the ad cuepoints, and publish.
//
// Build:   go build -o megaphone-publisher .
// Run:     ./megaphone-publisher                 (reads ./megaphone.toml)
//          ./megaphone-publisher -config /path/to/megaphone.toml
//          ./megaphone-publisher -dry-run        (no uploads / no API calls)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

func main() {
	configPath := flag.String("config", "megaphone.toml", "path to the config file")
	dryRun := flag.Bool("dry-run", false, "print what would happen without uploading or calling the API")
	noPublish := flag.Bool("no-publish", false, "do everything but leave the episode as a draft (don't set the pubdate / go live)")
	flag.Parse()

	if err := run(*configPath, *dryRun, *noPublish); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, dryRun, noPublish bool) error {
	if err := mustReadable(configPath); err != nil {
		return fmt.Errorf("can't open config file %q (put megaphone.toml next to the app, or pass -config): %w", configPath, err)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	in := bufio.NewReader(os.Stdin)
	client := NewClient(cfg.Megaphone)

	fmt.Println("Megaphone episode publisher")
	fmt.Println("===========================")

	// Smart defaults for season/episode from the most recent episode (the list
	// is newest-first, so the next episode defaults to latest + 1).
	defSeason, defEpisode := "", ""
	if latest, err := client.LatestEpisode(); err != nil {
		fmt.Printf("  (couldn't fetch the latest episode for numbering defaults: %v)\n", err)
	} else if latest != nil {
		defSeason = strconv.Itoa(latest.SeasonNumber)
		defEpisode = strconv.Itoa(latest.EpisodeNumber + 1)
		fmt.Printf("  (latest is S%d E%d %q → defaulting next to S%s E%s)\n",
			latest.SeasonNumber, latest.EpisodeNumber, latest.Title, defSeason, defEpisode)
	}

	// --- Gather the per-episode details (constants come from config) ---
	title := prompt(in, "Episode title", "")
	if title == "" {
		return fmt.Errorf("a title is required")
	}
	description := readDescription(in)
	cleanTitle := prompt(in, "Clean title", "")

	season, err := promptInt(in, "Season number", defSeason)
	if err != nil {
		return fmt.Errorf("season number: %w", err)
	}
	episodeNum, err := promptInt(in, "Episode number", defEpisode)
	if err != nil {
		return fmt.Errorf("episode number: %w", err)
	}
	episodeType := prompt(in, "Episode type (full/trailer/bonus)", cfg.Defaults.EpisodeType)
	ratingDef := "clean"
	if cfg.Defaults.ExplicitRating {
		ratingDef = "explicit"
	}
	explicit := strings.EqualFold(prompt(in, "Rating (clean/explicit)", ratingDef), "explicit")

	videoPath := prompt(in, "Path to the video file", "")
	videoPath = expandHome(strings.TrimSpace(videoPath))
	if err := mustReadable(videoPath); err != nil {
		return fmt.Errorf("can't read video file %q: %w", videoPath, err)
	}

	durStr := prompt(in, "Episode length (mm:ss or h:mm:ss)", "")
	durationSecs, err := parseClock(durStr)
	if err != nil {
		return fmt.Errorf("couldn't read episode length: %w", err)
	}

	var midrolls []int
	if durationSecs > cfg.Defaults.MinSecondsForAds {
		mr := prompt(in, "Mid-roll timestamps (comma separated, mm:ss — blank for none)", "")
		midrolls, err = parseTimestamps(mr)
		if err != nil {
			return fmt.Errorf("couldn't read mid-roll timestamps: %w", err)
		}
	} else {
		fmt.Printf("  (episode is under %s — skipping pre-roll and mid-roll, as per the rules)\n",
			fmtClock(cfg.Defaults.MinSecondsForAds))
	}

	cues := buildCuepoints(cfg.Defaults, durationSecs, midrolls)
	rating := "Clean"
	if explicit {
		rating = "Explicit"
	}

	// --- Confirm before doing anything irreversible ---
	fmt.Println("\nAbout to publish:")
	fmt.Printf("  Title:      %s\n", title)
	fmt.Printf("  Clean ttl:  %s\n", cleanTitle)
	fmt.Printf("  Subtitle:   %s\n", cfg.Defaults.Subtitle)
	fmt.Printf("  Season/Ep:  S%d E%d  (%s, %s)\n", season, episodeNum, episodeType, rating)
	fmt.Printf("  Link:       %s\n", cfg.Defaults.Link)
	fmt.Printf("  Video:      %s\n", videoPath)
	fmt.Printf("  Length:     %s\n", fmtClock(durationSecs))
	fmt.Printf("  Cuepoints:  %s\n", describeCues(cues))
	if dryRun {
		fmt.Println("\n[dry-run] stopping here — no upload, no API calls.")
		b, _ := json.MarshalIndent(cues, "  ", "  ")
		fmt.Printf("[dry-run] cuepoints payload:\n  %s\n", b)
		return nil
	}
	if strings.ToLower(prompt(in, "\nProceed? (y/N)", "N")) != "y" {
		fmt.Println("Aborted.")
		return nil
	}

	// --- 1. Upload the video to S3 and presign a URL Megaphone can fetch ---
	fmt.Println("\n[1/4] Uploading video to S3…")
	up, err := NewUploader(ctx, cfg.S3)
	if err != nil {
		return err
	}
	mediaURL, err := up.Upload(ctx, videoPath)
	if err != nil {
		return err
	}
	fmt.Println("  ✓ uploaded; Megaphone will fetch it from the presigned URL")

	// --- 2. Create the episode as a draft (media by URL, no pubdate) ---
	fmt.Println("[2/4] Creating episode (draft)…")
	ep, err := client.CreateEpisode(createEpisodeReq{
		Title:                  title,
		CleanTitle:             cleanTitle,
		Summary:                description,
		Subtitle:               cfg.Defaults.Subtitle,
		Link:                   cfg.Defaults.Link,
		Author:                 cfg.Defaults.Author,
		SeasonNumber:           season,
		EpisodeNumber:          episodeNum,
		EpisodeType:            episodeType,
		Explicit:               explicit,
		MediaFileURL:           mediaURL,
		BackgroundImageFileURL: cfg.Defaults.BackgroundImageFileURL,
		// Create as a draft with a placeholder pubdate (required by the API).
		// Step 4 flips draft->false to publish; -no-publish leaves it here.
		Pubdate: time.Now().UTC().Format(time.RFC3339),
		Draft:   true,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ episode created: %s\n", ep.ID)

	// --- 3. Wait for media processing to finish ---
	fmt.Println("[3/4] Waiting for Megaphone to process the video (this can take a while)…")
	if err := waitForProcessing(ctx, client, ep.ID); err != nil {
		return err
	}
	fmt.Println("  ✓ processing complete")

	// --- 4. Attach cuepoints, then publish (unless -no-publish) ---
	if len(cues) > 0 {
		fmt.Printf("[4/4] Adding %d cuepoint(s)…\n", len(cues))
		if err := client.SetCuepoints(ep.ID, cues); err != nil {
			return fmt.Errorf("setting cuepoints: %w", err)
		}
	}
	if noPublish {
		fmt.Printf("\n✓ Done. Episode %s left as a DRAFT (no pubdate set) — review it in Megaphone and publish when ready.\n", ep.ID)
		return nil
	}
	published, err := client.Publish(ep.ID, time.Now())
	if err != nil {
		return fmt.Errorf("publishing: %w", err)
	}
	fmt.Printf("\n✓ Done. Episode %s is now %q.\n", published.ID, published.Status)
	return nil
}

// waitForProcessing polls the episode until its media is ready. We poll every
// 30s to stay comfortably under the 60-requests-per-minute rate limit.
func waitForProcessing(ctx context.Context, client *Client, id string) error {
	const interval = 30 * time.Second
	start := time.Now()
	for {
		ep, err := client.GetEpisode(id)
		if err != nil {
			return err
		}
		switch ep.AudioFileStatus {
		case "success":
			return nil
		case "error":
			return fmt.Errorf("Megaphone reported a processing error for the media file")
		default: // no_audio | processing
			fmt.Printf("\r  status: %-12s (%s elapsed)", ep.AudioFileStatus, fmtClock(int(time.Since(start).Seconds())))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled while waiting (episode %s is still processing — it will finish on Megaphone's side)", id)
		case <-time.After(interval):
		}
	}
}

// buildCuepoints turns the rules into the cuepoint payload:
//   - postroll: always, PostrollAdCount slots at EOF
//   - preroll:  only if longer than MinSecondsForAds, PrerollAdCount slots at 0
//   - midroll:  only if longer than MinSecondsForAds, one break per timestamp
func buildCuepoints(d DefaultsConfig, durationSecs int, midrolls []int) []Cuepoint {
	var cues []Cuepoint
	long := durationSecs > d.MinSecondsForAds

	if long && d.PrerollAdCount > 0 {
		cues = append(cues, Cuepoint{
			CuepointType: "preroll",
			AdCount:      d.PrerollAdCount,
			StartTime:    0,
			AdSources:    autoSources(d.PrerollAdCount),
			Action:       "insert",
			IsActive:     true,
		})
	}
	if long {
		for _, ts := range midrolls {
			cues = append(cues, Cuepoint{
				CuepointType: "midroll",
				AdCount:      d.MidrollAdCount,
				StartTime:    ts,
				AdSources:    autoSources(d.MidrollAdCount),
				Action:       "insert",
				IsActive:     true,
			})
		}
	}
	if d.PostrollAdCount > 0 {
		cues = append(cues, Cuepoint{
			CuepointType: "postroll",
			AdCount:      d.PostrollAdCount,
			StartTime:    durationSecs, // EOF
			AdSources:    autoSources(d.PostrollAdCount),
			Action:       "insert",
			IsActive:     true,
		})
	}
	return cues
}

func autoSources(n int) []string {
	s := make([]string, n)
	for i := range s {
		s[i] = "auto"
	}
	return s
}

// ---- small input/format helpers ----

func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptInt asks for a whole number, with an optional default (blank = 0/unset).
func promptInt(in *bufio.Reader, label, def string) (int, error) {
	s := strings.TrimSpace(prompt(in, label, def))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a whole number", s)
	}
	return n, nil
}

// readDescription gets the episode description. Descriptions are often long
// HTML, so "@path" loads the text from a file instead of typing it inline.
func readDescription(in *bufio.Reader) string {
	s := prompt(in, "Episode description (HTML ok; or @path to load from a file)", "")
	if strings.HasPrefix(s, "@") {
		path := expandHome(strings.TrimSpace(s[1:]))
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("  (couldn't read %s: %v — using the literal text)\n", path, err)
			return s
		}
		return string(b)
	}
	return s
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// parseClock reads "mm:ss", "h:mm:ss", or plain seconds into seconds.
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", p)
		}
		total = total*60 + n
	}
	return total, nil
}

func parseTimestamps(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	var out []int
	for _, f := range fields {
		secs, err := parseClock(f)
		if err != nil {
			return nil, err
		}
		out = append(out, secs)
	}
	return out, nil
}

func fmtClock(secs int) string {
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func describeCues(cues []Cuepoint) string {
	if len(cues) == 0 {
		return "none"
	}
	var parts []string
	for _, c := range cues {
		parts = append(parts, fmt.Sprintf("%s×%d@%s", c.CuepointType, c.AdCount, fmtClock(c.StartTime)))
	}
	return strings.Join(parts, ", ")
}
