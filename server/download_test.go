package main

import (
	"reflect"
	"testing"
)

func TestCleanDownloadPaths(t *testing.T) {
	got := cleanDownloadPaths([]string{
		"index.html",
		"css/app.css",
		"",
		".",
		"..",
		"/etc/passwd",
		"../secret",
		"img/../secret",
		`windows\path.txt`,
		"C:/tmp/x",
		"C:tmp/x",
		"a:b",
		"about.html",
	})
	want := []string{"about.html", "css/app.css", "index.html"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanDownloadPaths = %#v, want %#v", got, want)
	}
}
