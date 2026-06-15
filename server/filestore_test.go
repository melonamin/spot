package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestLocalFileStoreListAndDelete(t *testing.T) {
	ctx := context.Background()
	store, err := NewLocalFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	a, err := store.Put(ctx, "demo", "alpha.txt", "text/plain", bytes.NewBufferString("a"), 1)
	if err != nil {
		t.Fatalf("put alpha: %v", err)
	}
	b, err := store.Put(ctx, "demo", "beta.txt", "text/plain", bytes.NewBufferString("bb"), 2)
	if err != nil {
		t.Fatalf("put beta: %v", err)
	}

	files, err := store.List(ctx, "demo")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 || files[0].Name != "alpha.txt" || files[1].Name != "beta.txt" {
		t.Fatalf("list = %+v, want alpha then beta", files)
	}
	if files[0].ID != a.ID || files[0].Size != 1 || files[1].Size != 2 {
		t.Errorf("list metadata = %+v", files)
	}

	// A site with no uploads lists empty, not an error.
	if other, err := store.List(ctx, "empty"); err != nil || len(other) != 0 {
		t.Errorf("list empty site = %v (%v), want none", other, err)
	}

	if err := store.Delete(ctx, "demo", a.ID, a.Name); err != nil {
		t.Fatalf("delete alpha: %v", err)
	}
	if _, _, err := store.Get(ctx, "demo", a.ID, a.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted = %v, want ErrNotFound", err)
	}
	// Deleting again is idempotent.
	if err := store.Delete(ctx, "demo", a.ID, a.Name); err != nil {
		t.Errorf("delete twice: %v", err)
	}
	// Malformed input fails closed without touching the store.
	if err := store.Delete(ctx, "demo", "not-an-id", a.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete malformed id = %v, want ErrNotFound", err)
	}

	files, err = store.List(ctx, "demo")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(files) != 1 || files[0].ID != b.ID {
		t.Fatalf("list after delete = %+v, want only beta", files)
	}

	// The deleted upload's id directory is pruned; the survivor's remains.
	if _, err := os.Stat(filepath.Join(store.root, "demo", a.ID)); !os.IsNotExist(err) {
		t.Errorf("deleted id directory %s still exists (stat err = %v)", a.ID, err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "demo", b.ID)); err != nil {
		t.Errorf("surviving id directory %s missing: %v", b.ID, err)
	}
}

// TestFileStoreDeleteRejectsMalformedKeys mirrors the Get check: Delete
// must fail closed before reaching the object store, so a nil client is
// safe here precisely because the rejection short-circuits the S3 call.
func TestFileStoreDeleteRejectsMalformedKeys(t *testing.T) {
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
			if err := store.Delete(context.Background(), tt.site, tt.id, tt.file); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Delete(%q,%q,%q) err = %v, want ErrNotFound", tt.site, tt.id, tt.file, err)
			}
		})
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
