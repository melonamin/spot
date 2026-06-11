package main

import "testing"

func TestSiteFromHost(t *testing.T) {
	const domain = "quick.localhost"
	tests := []struct {
		host string
		want string
	}{
		{"mysite.quick.localhost", "mysite"},
		{"mysite.quick.localhost:8443", "mysite"},
		{"MySite.Quick.Localhost", "mysite"},
		{"mysite.quick.localhost.", "mysite"},
		{"quick.localhost", ""},
		{"quick.localhost:8443", ""},
		{"a.b.quick.localhost", ""},
		{"evil.example.com", ""},
		{"notquick.localhost", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := siteFromHost(tt.host, domain); got != tt.want {
			t.Errorf("siteFromHost(%q, %q) = %q, want %q", tt.host, domain, got, tt.want)
		}
	}
}

func TestScopeFor(t *testing.T) {
	scope, err := scopeFor("mysite", "posts")
	if err != nil {
		t.Fatalf("scopeFor(mysite, posts): unexpected error %v", err)
	}
	if scope != "mysite" {
		t.Errorf("scopeFor(mysite, posts) = %q, want %q", scope, "mysite")
	}

	if _, err := scopeFor("", "posts"); err == nil {
		t.Error("scopeFor with empty site: want error, got nil")
	}
	for _, bad := range []string{"", "Posts", "po sts", "a/b", "x'", "verylong" + string(make([]byte, 64))} {
		if _, err := scopeFor("mysite", bad); err == nil {
			t.Errorf("scopeFor(mysite, %q): want error, got nil", bad)
		}
	}
}
