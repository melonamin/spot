package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInlineSafe(t *testing.T) {
	tests := []struct {
		contentType string
		want        bool
	}{
		{"image/png", true},
		{"image/jpeg", true},
		{"application/pdf", true},
		{"text/plain; charset=utf-8", true},
		{"audio/mpeg", true},
		{"video/mp4", true},
		{"text/html; charset=utf-8", false}, // the XSS vector
		{"image/svg+xml", false},            // SVG can carry script
		{"application/octet-stream", false},
		{"application/javascript", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := inlineSafe(tt.contentType); got != tt.want {
			t.Errorf("inlineSafe(%q) = %v, want %v", tt.contentType, got, tt.want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"report.pdf", "report.pdf"},
		{"my photo (1).png", "my_photo_1_.png"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\sasha\doc.txt`, "doc.txt"},
		{"...", "file"},
		{"", "file"},
		{"üñïçødé.txt", "d_.txt"},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.in); got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestFileStoreGetRejectsMalformedKeys checks that Get fails closed on a
// malformed site, id, or name before reaching the object store. A nil
// client is safe here precisely because the rejection short-circuits the
// S3 call; a regression that dropped the check would nil-panic instead.
func TestFileStoreGetRejectsMalformedKeys(t *testing.T) {
	store := &FileStore{}
	validID := strings.Repeat("a", 32)
	cases := []struct {
		name           string
		site, id, file string
	}{
		{"bad site", "Bad Site", validID, "report.pdf"},
		{"bad id length", "demo", "abc", "report.pdf"},
		{"bad id chars", "demo", strings.Repeat("g", 32), "report.pdf"},
		{"path traversal name", "demo", validID, "../../etc/passwd"},
		{"slash in name", "demo", validID, "sub/report.pdf"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rc, _, err := store.Get(context.Background(), tt.site, tt.id, tt.file)
			if rc != nil {
				rc.Close()
			}
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(%q,%q,%q) err = %v, want ErrNotFound", tt.site, tt.id, tt.file, err)
			}
		})
	}
}
