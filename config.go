// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Rahoul Baruah

package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config is the whole megaphone.toml file. One file holds the secrets, the
// connection details, and the "standard format" defaults that repeat every
// week, so a non-technical user only has to answer the few per-episode prompts.
type Config struct {
	Megaphone MegaphoneConfig `toml:"megaphone"`
	S3        S3Config        `toml:"s3"`
	Defaults  DefaultsConfig  `toml:"defaults"`
}

type MegaphoneConfig struct {
	Token     string `toml:"token"`
	NetworkID string `toml:"network_id"`
	PodcastID string `toml:"podcast_id"` // the show (e.g. World Cup Wonders); created once
	BaseURL   string `toml:"base_url"`   // default https://cms.megaphone.fm/api
}

type S3Config struct {
	Bucket          string `toml:"bucket"`
	Region          string `toml:"region"`
	KeyPrefix       string `toml:"key_prefix"`        // e.g. "uploads/" (optional)
	Profile         string `toml:"profile"`           // named AWS profile, OR set the keys below
	AccessKeyID     string `toml:"access_key_id"`     // optional; blank = use profile / env
	SecretAccessKey string `toml:"secret_access_key"` // optional
	PresignHours    int    `toml:"presign_hours"`     // how long Megaphone has to fetch the file
}

type DefaultsConfig struct {
	// Constant per-show fields (same every episode)
	Subtitle               string `toml:"subtitle"`                  // e.g. "1865: The ORIGINAL Nottingham Forest Podcast"
	Link                   string `toml:"link"`                      // episode URL, e.g. "https://eighteensixtyfive.football"
	Author                 string `toml:"author"`                    // optional; blank = leave to podcast default
	BackgroundImageFileURL string `toml:"background_image_file_url"` // optional episode artwork

	// Per-episode defaults (user can override at the prompt)
	EpisodeType   string `toml:"episode_type"`   // full | trailer | bonus  (default full)
	ExplicitRating bool  `toml:"explicit_rating"` // false = Clean (default), true = Explicit

	// Cuepoint rules
	MinSecondsForAds int `toml:"min_seconds_for_ads"` // e.g. 300 (5 minutes)
	PrerollAdCount   int `toml:"preroll_ad_count"`    // e.g. 3
	PostrollAdCount  int `toml:"postroll_ad_count"`   // e.g. 3
	MidrollAdCount   int `toml:"midroll_ad_count"`    // ad slots per midroll break, e.g. 1
}

func loadConfig(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Sensible fallbacks so the config file can stay short.
	if c.Megaphone.BaseURL == "" {
		c.Megaphone.BaseURL = "https://cms.megaphone.fm/api"
	}
	if c.S3.PresignHours == 0 {
		c.S3.PresignHours = 6
	}
	if c.Defaults.MinSecondsForAds == 0 {
		c.Defaults.MinSecondsForAds = 300
	}
	if c.Defaults.MidrollAdCount == 0 {
		c.Defaults.MidrollAdCount = 1
	}
	if c.Defaults.EpisodeType == "" {
		c.Defaults.EpisodeType = "full"
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var missing []string
	if c.Megaphone.Token == "" {
		missing = append(missing, "megaphone.token")
	}
	if c.Megaphone.NetworkID == "" {
		missing = append(missing, "megaphone.network_id")
	}
	if c.Megaphone.PodcastID == "" {
		missing = append(missing, "megaphone.podcast_id")
	}
	if c.S3.Bucket == "" {
		missing = append(missing, "s3.bucket")
	}
	if c.S3.Region == "" {
		missing = append(missing, "s3.region")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config is missing required values: %v", missing)
	}
	return nil
}

func mustReadable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}
