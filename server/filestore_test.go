package main

import "testing"

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
