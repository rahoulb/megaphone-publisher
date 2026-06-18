// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Rahoul Baruah

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to the Megaphone CMS API.
// Auth is the header:  Authorization: Token token="YOUR_TOKEN"
// Base URL:            https://cms.megaphone.fm/api
// Rate limit:          60 requests / minute.
type Client struct {
	http      *http.Client
	baseURL   string
	token     string
	networkID string
	podcastID string
}

func NewClient(cfg MegaphoneConfig) *Client {
	return &Client{
		http:      &http.Client{Timeout: 60 * time.Second},
		baseURL:   cfg.BaseURL,
		token:     cfg.Token,
		networkID: cfg.NetworkID,
		podcastID: cfg.PodcastID,
	}
}

// Episode is the subset of the Megaphone episode object we care about.
type Episode struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Status          string `json:"status"`          // not_ready | scheduled | published
	AudioFileStatus string `json:"audioFileStatus"` // no_audio | processing | success | error
	Pubdate         string `json:"pubdate"`
	SeasonNumber    int    `json:"seasonNumber"`
	EpisodeNumber   int    `json:"episodeNumber"`
}

func (c *Client) do(method, url string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Token token=%q", c.token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %d: %s", method, url, resp.StatusCode, truncate(string(respBody), 500))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decoding response from %s: %w (body: %s)", url, err, truncate(string(respBody), 300))
		}
	}
	return nil
}

func (c *Client) episodesURL() string {
	return fmt.Sprintf("%s/networks/%s/podcasts/%s/episodes", c.baseURL, c.networkID, c.podcastID)
}

func (c *Client) episodeURL(id string) string {
	return fmt.Sprintf("%s/networks/%s/podcasts/%s/episodes/%s", c.baseURL, c.networkID, c.podcastID, id)
}

// CreateEpisode creates the episode as a DRAFT: media is supplied as a URL that
// Megaphone fetches, and NO pubdate is set (an episode without a pubdate is a
// draft). Megaphone then begins processing the media asynchronously.
type createEpisodeReq struct {
	Title                  string `json:"title"`
	CleanTitle             string `json:"cleanTitle,omitempty"`
	Summary                string `json:"summary,omitempty"` // the episode description (HTML ok)
	Subtitle               string `json:"subtitle,omitempty"`
	Link                   string `json:"link,omitempty"`
	Author                 string `json:"author,omitempty"`
	SeasonNumber           int    `json:"seasonNumber,omitempty"`
	EpisodeNumber          int    `json:"episodeNumber,omitempty"`
	EpisodeType            string `json:"episodeType,omitempty"` // full | trailer | bonus
	Explicit               bool   `json:"explicit"`              // sent always (false = Clean)
	MediaFileURL           string `json:"mediaFileUrl"`
	BackgroundImageFileURL string `json:"backgroundImageFileUrl,omitempty"`
	// Megaphone requires a pubdate even for drafts; `draft: true` is what keeps
	// the episode out of the live feed. Publishing flips draft to false.
	Pubdate string `json:"pubdate"`
	Draft   bool   `json:"draft"`
}

func (c *Client) CreateEpisode(in createEpisodeReq) (*Episode, error) {
	var ep Episode
	if err := c.do(http.MethodPost, c.episodesURL(), in, &ep); err != nil {
		return nil, err
	}
	if ep.ID == "" {
		return nil, fmt.Errorf("episode created but no id returned")
	}
	return &ep, nil
}

// LatestEpisode returns the most recently published episode (the list is
// ordered newest-first), used to auto-default the season and episode numbers.
// Returns nil (no error) if the podcast has no episodes yet.
func (c *Client) LatestEpisode() (*Episode, error) {
	var eps []Episode
	url := c.episodesURL() + "?per_page=1&page=1"
	if err := c.do(http.MethodGet, url, nil, &eps); err != nil {
		return nil, err
	}
	if len(eps) == 0 {
		return nil, nil
	}
	return &eps[0], nil
}

func (c *Client) GetEpisode(id string) (*Episode, error) {
	var ep Episode
	if err := c.do(http.MethodGet, c.episodeURL(id), nil, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// Publish takes the episode out of draft (draft:false) and sets the pubdate,
// which makes it live (or scheduled if the pubdate is in the future).
func (c *Client) Publish(id string, pubdate time.Time) (*Episode, error) {
	body := map[string]any{
		"pubdate": pubdate.UTC().Format(time.RFC3339),
		"draft":   false,
	}
	var ep Episode
	if err := c.do(http.MethodPut, c.episodeURL(id), body, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// Cuepoint matches the cuepoints_batch schema.
type Cuepoint struct {
	CuepointType string   `json:"cuepointType"` // preroll | midroll | postroll
	AdCount      int      `json:"adCount"`
	StartTime    int      `json:"startTime"` // seconds from 0 (EOF for postroll)
	AdSources    []string `json:"adSources"` // one "auto" per ad slot
	Action       string   `json:"action"`    // "insert"
	IsActive     bool     `json:"isActive"`
	Title        string   `json:"title,omitempty"`
}

// SetCuepoints REPLACES all cuepoints (the PUT variant). Megaphone auto-creates
// pre/post-roll cuepoints from the podcast defaults the moment an episode is
// created, so appending (POST) would duplicate them. We replace instead so the
// final set is exactly what we computed.
//
// PUT is "destructive" in that it also clears Sponsorship cuepoints — but this
// runs at creation time on a brand-new episode, before any sponsorship could
// have been attached, so there is nothing to preserve.
func (c *Client) SetCuepoints(episodeID string, cues []Cuepoint) error {
	url := fmt.Sprintf("%s/episodes/%s/cuepoints_batch", c.baseURL, episodeID)
	return c.do(http.MethodPut, url, cues, nil)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
