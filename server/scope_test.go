package main

import "testing"

func TestSiteFromHost(t *testing.T) {
	const domain = "spot.localhost"
	tests := []struct {
		host string
		want string
	}{
		{"mysite.spot.localhost", "mysite"},
		{"mysite.spot.localhost:8443", "mysite"},
		{"MySite.Spot.Localhost", "mysite"},
		{"mysite.spot.localhost.", "mysite"},
		{"spot.localhost", ""},
		{"spot.localhost:8443", ""},
		{"a.b.spot.localhost", ""},
		{"evil.example.com", ""},
		{"notspot.localhost", ""},
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

	// shared-* collections live in one global namespace for all sites.
	for _, site := range []string{"mysite", "othersite"} {
		scope, err := scopeFor(site, "shared-libs")
		if err != nil {
			t.Fatalf("scopeFor(%s, shared-libs): unexpected error %v", site, err)
		}
		if scope != sharedScope {
			t.Errorf("scopeFor(%s, shared-libs) = %q, want %q", site, scope, sharedScope)
		}
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
