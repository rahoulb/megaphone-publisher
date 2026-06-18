// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Rahoul Baruah

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Uploader pushes the (large) video to S3 and hands back a presigned GET URL
// that Megaphone can fetch the file from. Megaphone pulls media by URL — it does
// not accept a pushed file — so S3 is the staging ground.
type Uploader struct {
	client    *s3.Client
	presigner *s3.PresignClient
	bucket    string
	keyPrefix string
	presign   time.Duration
}

func NewUploader(ctx context.Context, cfg S3Config) (*Uploader, error) {
	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts, awsconfig.WithRegion(cfg.Region))
	if cfg.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg)
	return &Uploader{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    cfg.Bucket,
		keyPrefix: cfg.KeyPrefix,
		presign:   time.Duration(cfg.PresignHours) * time.Hour,
	}, nil
}

// progressReader wraps the file so we can print upload progress for a 5 GB file.
type progressReader struct {
	file  *os.File
	total int64
	read  int64
	last  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.file.Read(b)
	if n > 0 {
		done := atomic.AddInt64(&p.read, int64(n))
		// Print roughly every whole percent.
		pct := done * 100 / p.total
		if pct != atomic.LoadInt64(&p.last) {
			atomic.StoreInt64(&p.last, pct)
			fmt.Printf("\r  uploading… %3d%% (%s / %s)", pct, humanBytes(done), humanBytes(p.total))
		}
	}
	return n, err
}

func (p *progressReader) Seek(offset int64, whence int) (int64, error) {
	// The upload manager seeks to compute part boundaries; reset the counter on
	// a seek-to-start so the percentage stays sane.
	pos, err := p.file.Seek(offset, whence)
	if offset == 0 && whence == 0 {
		atomic.StoreInt64(&p.read, 0)
	}
	return pos, err
}

// Upload uploads the local file and returns a presigned GET URL.
func (u *Uploader) Upload(ctx context.Context, localPath string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	key := u.keyPrefix + filepath.Base(localPath)
	key = strings.TrimPrefix(key, "/")

	up := manager.NewUploader(u.client, func(m *manager.Uploader) {
		m.PartSize = 64 * 1024 * 1024 // 64 MB parts -> well within the 10k-part limit for 5 GB
		m.Concurrency = 4
	})

	pr := &progressReader{file: f, total: info.Size()}
	_, err = up.Upload(ctx, &s3.PutObjectInput{
		Bucket: awssdk.String(u.bucket),
		Key:    awssdk.String(key),
		Body:   pr,
	})
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("uploading to s3://%s/%s: %w", u.bucket, key, err)
	}

	req, err := u.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: awssdk.String(u.bucket),
		Key:    awssdk.String(key),
	}, s3.WithPresignExpires(u.presign))
	if err != nil {
		return "", fmt.Errorf("presigning: %w", err)
	}
	return req.URL, nil
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
