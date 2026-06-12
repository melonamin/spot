package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var filenameSafeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// FileStore holds site file uploads in an S3 bucket. Uploads go through
// the server so browsers never see storage credentials.
type FileStore struct {
	client *minio.Client
	bucket string
}

func NewFileStore(endpoint, accessKey, secretKey, bucket string) (*FileStore, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
	})
	if err != nil {
		return nil, fmt.Errorf("file store client: %w", err)
	}
	return &FileStore{client: client, bucket: bucket}, nil
}

type StoredFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
}

func (f *FileStore) Put(ctx context.Context, site, filename, contentType string, r io.Reader, size int64) (StoredFile, error) {
	id, err := newFileID()
	if err != nil {
		return StoredFile{}, err
	}
	name := sanitizeFilename(filename)
	key := site + "/" + id + "/" + name
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = f.client.PutObject(ctx, f.bucket, key, r, size,
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return StoredFile{}, fmt.Errorf("store file %s: %w", key, err)
	}
	return StoredFile{
		ID:          id,
		Name:        name,
		Size:        size,
		ContentType: contentType,
		URL:         "/api/files/" + key,
	}, nil
}

// Get returns the object stream and its content type. The caller closes
// the reader.
func (f *FileStore) Get(ctx context.Context, site, id, name string) (io.ReadCloser, string, error) {
	key := site + "/" + id + "/" + name
	obj, err := f.client.GetObject(ctx, f.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("get file %s: %w", key, err)
	}
	stat, err := obj.Stat()
	if err != nil {
		closeErr := obj.Close()
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return nil, "", ErrNotFound
		}
		if closeErr != nil {
			return nil, "", fmt.Errorf("stat file %s: %w", key, errors.Join(err, closeErr))
		}
		return nil, "", fmt.Errorf("stat file %s: %w", key, err)
	}
	return obj, stat.ContentType, nil
}

// RemoveSite deletes every upload stored for a site. Used when the site
// is deleted, so a later claimant of the name cannot serve or inherit
// the old owner's uploads.
func (f *FileStore) RemoveSite(ctx context.Context, site string) error {
	prefix := site + "/"
	for obj := range f.client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("list uploads for %s: %w", site, obj.Err)
		}
		if err := f.client.RemoveObject(ctx, f.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("remove upload %s: %w", obj.Key, err)
		}
	}
	return nil
}

func newFileID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate file id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// inlineSafe reports whether a content type may be rendered inline by a
// browser without risk of script execution. SVG and HTML are
// deliberately excluded — both can carry script — so they download
// instead. Images, PDFs, plain text, and audio/video render inline so
// sites can embed them directly.
func inlineSafe(contentType string) bool {
	media := contentType
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = media[:i]
	}
	media = strings.TrimSpace(strings.ToLower(media))
	switch media {
	case "image/png", "image/jpeg", "image/gif", "image/webp",
		"application/pdf", "text/plain":
		return true
	}
	return strings.HasPrefix(media, "audio/") || strings.HasPrefix(media, "video/")
}

// sanitizeFilename keeps the base name with a conservative character
// set, so object keys and download paths stay unambiguous.
func sanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = filenameSafeRe.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		return "file"
	}
	if len(name) > 128 {
		name = name[len(name)-128:]
	}
	return name
}
