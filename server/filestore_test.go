package main

import "testing"

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
