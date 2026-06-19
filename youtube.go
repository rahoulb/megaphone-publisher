// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Rahoul Baruah

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// YT wraps an authenticated YouTube Data API service.
type YT struct {
	svc *youtube.Service
	cfg YouTubeConfig
}

// oauthConfig builds the OAuth config from the downloaded client-secret JSON.
// Scopes: upload (insert) + full youtube (videos.update, playlists, playlistItems).
func oauthConfig(cfg YouTubeConfig) (*oauth2.Config, error) {
	b, err := os.ReadFile(expandHome(cfg.ClientSecretFile))
	if err != nil {
		return nil, fmt.Errorf("reading client secret %q: %w", cfg.ClientSecretFile, err)
	}
	oc, err := google.ConfigFromJSON(b, youtube.YoutubeUploadScope, youtube.YoutubeScope)
	if err != nil {
		return nil, fmt.Errorf("parsing client secret: %w", err)
	}
	return oc, nil
}

// newYT builds an authenticated service from the cached token. If there is no
// cached token it errors and tells the user to run -youtube-login first (we do
// NOT pop a browser in the middle of a publish run).
func newYT(ctx context.Context, cfg YouTubeConfig) (*YT, error) {
	oc, err := oauthConfig(cfg)
	if err != nil {
		return nil, err
	}
	tok, err := tokenFromFile(cfg.TokenCacheFile)
	if err != nil {
		return nil, fmt.Errorf("not signed in to YouTube (run `-youtube-login` once): %w", err)
	}
	client := oc.Client(ctx, tok) // auto-refreshes using the refresh token
	svc, err := youtube.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("creating YouTube service: %w", err)
	}
	return &YT{svc: svc, cfg: cfg}, nil
}

// Login runs the interactive browser sign-in and caches the token.
func (cfg YouTubeConfig) Login(ctx context.Context) error {
	oc, err := oauthConfig(cfg)
	if err != nil {
		return err
	}
	tok, err := tokenFromWeb(ctx, oc)
	if err != nil {
		return err
	}
	if err := saveToken(cfg.TokenCacheFile, tok); err != nil {
		return err
	}
	fmt.Printf("✓ Signed in to YouTube; token cached at %s\n", cfg.TokenCacheFile)
	return nil
}

// tokenFromWeb runs the installed-app loopback flow: spin up a local server,
// open the browser to Google's consent screen, catch the redirect, exchange the
// code for a token.
func tokenFromWeb(ctx context.Context, oc *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting local callback server: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	oc.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state := randomState()
	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			resCh <- result{err: fmt.Errorf("oauth state mismatch")}
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			resCh <- result{err: fmt.Errorf("authorization failed: %s", e)}
			return
		}
		fmt.Fprintln(w, "Signed in. You can close this tab and return to the terminal.")
		resCh <- result{code: r.URL.Query().Get("code")}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	// AccessTypeOffline guarantees a refresh token. prompt="select_account consent"
	// forces the account/channel picker — important for Brand Accounts, so you
	// get the "Choose a channel" step instead of silently reusing a remembered
	// (personal) channel.
	authURL := oc.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "select_account consent"),
	)
	fmt.Println("Opening your browser to sign in to YouTube…")
	fmt.Printf("If it doesn't open, visit this URL:\n  %s\n", authURL)
	_ = openBrowser(authURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := oc.Exchange(ctx, res.code)
		if err != nil {
			return nil, fmt.Errorf("exchanging code for token: %w", err)
		}
		return tok, nil
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for sign-in")
	}
}

// ---- Playlists ----

type Playlist struct {
	ID    string
	Title string
}

// ListPlaylists returns the signed-in channel's playlists (what the account
// calls "categories", e.g. "1865 Group of Death").
func (y *YT) ListPlaylists() ([]Playlist, error) {
	var out []Playlist
	call := y.svc.Playlists.List([]string{"snippet"}).Mine(true).MaxResults(50)
	for {
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("listing playlists: %w", err)
		}
		for _, p := range resp.Items {
			out = append(out, Playlist{ID: p.Id, Title: p.Snippet.Title})
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		call = call.PageToken(resp.NextPageToken)
	}
}

func (y *YT) AddToPlaylist(videoID, playlistID string) error {
	_, err := y.svc.PlaylistItems.Insert([]string{"snippet"}, &youtube.PlaylistItem{
		Snippet: &youtube.PlaylistItemSnippet{
			PlaylistId: playlistID,
			ResourceId: &youtube.ResourceId{Kind: "youtube#video", VideoId: videoID},
		},
	}).Do()
	if err != nil {
		return fmt.Errorf("adding video to playlist %s: %w", playlistID, err)
	}
	return nil
}

// ---- Video insert / wait / publish (ready for the next increment) ----

type YTVideoMeta struct {
	Title       string
	Description string
	Tags        []string
}

// InsertVideo uploads the file as a PRIVATE video (resumable upload) and returns
// the new video id. Privacy stays private until WaitForProcessing + SetPublic.
func (y *YT) InsertVideo(ctx context.Context, meta YTVideoMeta, filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	video := &youtube.Video{
		Snippet: &youtube.VideoSnippet{
			Title:                meta.Title,
			Description:          meta.Description,
			Tags:                 meta.Tags,
			CategoryId:           y.cfg.CategoryID,
			DefaultLanguage:      y.cfg.DefaultLanguage,
			DefaultAudioLanguage: y.cfg.DefaultAudioLanguage,
		},
		Status: &youtube.VideoStatus{
			PrivacyStatus:           "private",
			SelfDeclaredMadeForKids: y.cfg.MadeForKids,
			License:                 y.cfg.License,
			Embeddable:              true,
			// Force-send the booleans so `false` (e.g. Made-for-kids: NO) is
			// actually transmitted rather than omitted.
			ForceSendFields: []string{"SelfDeclaredMadeForKids", "Embeddable"},
		},
	}

	call := y.svc.Videos.Insert([]string{"snippet", "status"}, video).
		NotifySubscribers(y.cfg.NotifySubscribers).
		ProgressUpdater(func(current, total int64) {
			if total <= 0 {
				total = info.Size()
			}
			if total > 0 {
				fmt.Printf("\r  uploading to YouTube… %3d%% (%s / %s)",
					current*100/total, humanBytes(current), humanBytes(total))
			}
		})

	out, err := call.Media(f, googleapi.ChunkSize(16*1024*1024)).Do()
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("uploading video to YouTube: %w", err)
	}
	return out.Id, nil
}

// WaitForProcessing polls until YouTube has finished its initial processing/scan.
// Returns an error (and does not publish) if the video is rejected or fails.
func (y *YT) WaitForProcessing(ctx context.Context, videoID string) error {
	const interval = 30 * time.Second
	start := time.Now()
	for {
		resp, err := y.svc.Videos.List([]string{"status", "processingDetails"}).Id(videoID).Do()
		if err != nil {
			return err
		}
		if len(resp.Items) == 0 {
			return fmt.Errorf("video %s not found while polling", videoID)
		}
		v := resp.Items[0]
		switch v.Status.UploadStatus {
		case "processed":
			return nil
		case "rejected":
			return fmt.Errorf("YouTube rejected the video (reason: %s)", v.Status.RejectionReason)
		case "failed":
			return fmt.Errorf("YouTube processing failed (reason: %s)", v.Status.FailureReason)
		default: // uploaded | processing
			ps := ""
			if v.ProcessingDetails != nil {
				ps = " / " + v.ProcessingDetails.ProcessingStatus
			}
			fmt.Printf("\r  YouTube status: %-10s%s (%s elapsed)", v.Status.UploadStatus, ps, fmtClock(int(time.Since(start).Seconds())))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// SetPublic flips the video from private to public after the scan has passed.
func (y *YT) SetPublic(videoID string) error {
	_, err := y.svc.Videos.Update([]string{"status"}, &youtube.Video{
		Id: videoID,
		Status: &youtube.VideoStatus{
			PrivacyStatus:           "public",
			SelfDeclaredMadeForKids: y.cfg.MadeForKids,
			License:                 y.cfg.License,
			Embeddable:              true,
			ForceSendFields:         []string{"SelfDeclaredMadeForKids", "Embeddable"},
		},
	}).Do()
	if err != nil {
		return fmt.Errorf("publishing (set public): %w", err)
	}
	return nil
}

// ---- token persistence + helpers ----

func tokenFromFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(expandHome(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	f, err := os.OpenFile(expandHome(path), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("caching token: %w", err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
