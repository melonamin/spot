package main

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingDeployAuth struct {
	err    error
	action string
	auths  []string
	events []DeployAuditEvent
}

func (a *recordingDeployAuth) AuthorizeDeploy(_ context.Context, site string, actor Identity) (DeployAuthorization, error) {
	a.auths = append(a.auths, site+":"+actor.Email+":"+actor.PeerIP)
	if a.err != nil {
		return DeployAuthorization{}, a.err
	}
	action := a.action
	if action == "" {
		action = "update"
	}
	return DeployAuthorization{Action: action}, nil
}

func (a *recordingDeployAuth) RecordDeploy(_ context.Context, event DeployAuditEvent) error {
	a.events = append(a.events, event)
	return nil
}

func TestSiteNameRe(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"demo", true},
		{"my-site", true},
		{"a", true},
		{"123", true},
		{"intent-lunch", true},
		{strings.Repeat("a", 63), true},
		{"", false},
		{"-demo", false},
		{"demo-", false},
		{"Demo", false},
		{"my site", false},
		{"my.site", false},
		{strings.Repeat("a", 64), false},
	}
	for _, tt := range tests {
		if got := siteNameRe.MatchString(tt.name); got != tt.want {
			t.Errorf("siteNameRe.MatchString(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCleanSitePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"index.html", "index.html", false},
		{"css/app.css", "css/app.css", false},
		{"/leading/slash.js", "leading/slash.js", false},
		{"with space.html", "with space.html", false},
		{"", "", true},
		{"/", "", true},
		{`win\style.css`, "", true},
		{"../escape.html", "", true},
		{"a/../b.html", "", true},
		{"a/./b.html", "", true},
		{"a//b.html", "", true},
		{"a/b.html/", "", true},
		{"a:b.html", "", true},
		{"bad\nname.html", "", true},
		{strings.Repeat("a", 513), "", true},
	}
	for _, tt := range tests {
		got, err := cleanSitePath(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("cleanSitePath(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("cleanSitePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestJunkPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".DS_Store", true},
		{"sub/.DS_Store", true},
		{".git/config", true},
		{"node_modules/lib/x.js", true},
		{"Thumbs.db", true},
		{".env", true},
		{"index.html", false},
		{"_access.json", false},
		{"css/app.css", false},
		{"my.file.html", false},
	}
	for _, tt := range tests {
		if got := junkPath(tt.path); got != tt.want {
			t.Errorf("junkPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestNormalizeDeploy(t *testing.T) {
	paths := func(files []deployFile) []string {
		var out []string
		for _, f := range files {
			out = append(out, f.path)
		}
		return out
	}
	files := func(in ...string) []deployFile {
		var out []deployFile
		for _, p := range in {
			out = append(out, deployFile{path: p})
		}
		return out
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	tests := []struct {
		name    string
		in      []deployFile
		want    []string
		wantErr string
	}{
		{
			name: "index at root passes through",
			in:   files("index.html", "css/app.css"),
			want: []string{"index.html", "css/app.css"},
		},
		{
			name: "folder wrapping is stripped",
			in:   files("mysite/index.html", "mysite/css/app.css"),
			want: []string{"index.html", "css/app.css"},
		},
		{
			name: "nested wrapping is stripped repeatedly",
			in:   files("a/b/index.html", "a/b/app.js"),
			want: []string{"index.html", "app.js"},
		},
		{
			name: "junk is dropped before root detection",
			in:   files("mysite/index.html", "mysite/.git/config", "mysite/node_modules/x.js"),
			want: []string{"index.html"},
		},
		{
			name:    "missing index.html",
			in:      files("about.html", "css/app.css"),
			wantErr: "index.html",
		},
		{
			name:    "two roots without index",
			in:      files("a/index.html", "b/index.html"),
			wantErr: "index.html",
		},
		{
			name:    "duplicate paths",
			in:      files("index.html", "index.html"),
			wantErr: "duplicate",
		},
		{
			name:    "file and directory collide",
			in:      files("index.html", "docs", "docs/index.html"),
			wantErr: "conflicts",
		},
		{
			name:    "collide after folder wrapping",
			in:      files("site/index.html", "site/docs", "site/docs/index.html"),
			wantErr: "conflicts",
		},
		{
			name:    "only junk",
			in:      files(".DS_Store"),
			wantErr: "no files",
		},
		{
			name:    "empty deploy",
			in:      nil,
			wantErr: "no files",
		},
		{
			name:    "traversal is rejected",
			in:      files("../../etc/passwd"),
			wantErr: ".. segments",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeDeploy(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equal(paths(got), tt.want) {
				t.Errorf("paths = %v, want %v", paths(got), tt.want)
			}
		})
	}
}

// newTestSiteStore returns a local store for tests that are not asserting
// storage behavior.
func newTestSiteStore(t *testing.T) SiteStorage {
	t.Helper()
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatalf("site store: %v", err)
	}
	return sites
}

type failingSiteStore struct{}

func (f failingSiteStore) Put(context.Context, string, string, string, []byte) error {
	return io.ErrUnexpectedEOF
}
func (f failingSiteStore) List(context.Context, string) ([]string, error) {
	return nil, io.ErrUnexpectedEOF
}
func (f failingSiteStore) Open(context.Context, string, string) (io.ReadCloser, SiteFileInfo, error) {
	return nil, SiteFileInfo{}, io.ErrUnexpectedEOF
}
func (f failingSiteStore) Remove(context.Context, string, string) error { return io.ErrUnexpectedEOF }

type failingFileStore struct{}

func (f failingFileStore) Put(context.Context, string, string, string, io.Reader, int64) (StoredFile, error) {
	return StoredFile{}, io.ErrUnexpectedEOF
}
func (f failingFileStore) Get(context.Context, string, string, string) (io.ReadCloser, string, error) {
	return nil, "", io.ErrUnexpectedEOF
}
func (f failingFileStore) RemoveSite(context.Context, string) error { return io.ErrUnexpectedEOF }

func deployRequest(t *testing.T, host, site string, files map[string]string) *http.Request {
	t.Helper()
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if site != "" {
		if err := writer.WriteField("site", site); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="files"; filename="`+path+`"`)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			t.Fatal(err)
		}
	}
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "http://spot-api/api/deploy", &form)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Forwarded-Host", host)
	return req
}

func TestDeployValidation(t *testing.T) {
	srv := &Server{
		sites:          newTestSiteStore(t),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		// Every request here comes from the same test client IP; the
		// production burst of 3 would throttle the later cases.
		deployLimit: NewRateLimiter(1000, 1000),
	}

	call := func(req *http.Request) (int, string) {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, body := call(deployRequest(t, "demo.spot.localhost", "demo",
		map[string]string{"index.html": "<h1>hi</h1>"})); code != http.StatusBadRequest ||
		!strings.Contains(body, "platform root") {
		t.Errorf("deploy from site subdomain = %d %s, want 400 platform root", code, body)
	}
	if code, body := call(deployRequest(t, "spot.localhost", "Bad Name",
		map[string]string{"index.html": "<h1>hi</h1>"})); code != http.StatusBadRequest ||
		!strings.Contains(body, "site name") {
		t.Errorf("bad site name = %d %s, want 400 site name", code, body)
	}
	if code, body := call(deployRequest(t, "spot.localhost", "",
		map[string]string{"index.html": "<h1>hi</h1>"})); code != http.StatusBadRequest ||
		!strings.Contains(body, "site name") {
		t.Errorf("missing site name = %d %s, want 400 site name", code, body)
	}
	if code, body := call(deployRequest(t, "spot.localhost", "demo",
		map[string]string{"about.html": "<h1>hi</h1>"})); code != http.StatusBadRequest ||
		!strings.Contains(body, "index.html") {
		t.Errorf("missing index.html = %d %s, want 400 index.html", code, body)
	}
	if code, body := call(deployRequest(t, "spot.localhost", "demo",
		map[string]string{"../../etc/passwd": "x", "index.html": "y"})); code != http.StatusBadRequest ||
		!strings.Contains(body, "..") {
		t.Errorf("traversal = %d %s, want 400 ..", code, body)
	}

	plain := httptest.NewRequest(http.MethodPost, "http://spot-api/api/deploy",
		strings.NewReader("not multipart"))
	plain.Header.Set("X-Forwarded-Host", "spot.localhost")
	if code, body := call(plain); code != http.StatusBadRequest ||
		!strings.Contains(body, "multipart") {
		t.Errorf("non-multipart = %d %s, want 400 multipart", code, body)
	}
}

func TestDeployWithoutStore(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t)}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "demo",
		map[string]string{"index.html": "<h1>hi</h1>"}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("deploy without store = %d, want 503", rec.Code)
	}
}

func TestDeployRequiresIdentity(t *testing.T) {
	auth := &recordingDeployAuth{}
	srv := &Server{
		sites:          newTestSiteStore(t),
		deployAuth:     auth,
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
	req := deployRequest(t, "spot.localhost", "demo", map[string]string{"index.html": "<h1>hi</h1>"})
	req.Header.Set("X-Forwarded-For", "100.64.0.7")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("deploy without identity resolver = %d, want 503", rec.Code)
	}
	if len(auth.auths) != 0 || len(auth.events) != 0 {
		t.Fatalf("deploy auth touched without identity: auths=%v events=%v", auth.auths, auth.events)
	}
}

func TestDeployForbiddenIsAuditedBeforeStorage(t *testing.T) {
	auth := &recordingDeployAuth{err: ErrDeployForbidden}
	srv := &Server{
		sites:          newTestSiteStore(t),
		deployAuth:     auth,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
	req := deployRequest(t, "spot.localhost", "demo", map[string]string{"index.html": "<h1>hi</h1>"})
	req.Header.Set("X-Forwarded-For", "100.64.0.7")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden deploy = %d, want 403", rec.Code)
	}
	if len(auth.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(auth.events))
	}
	event := auth.events[0]
	if event.Status != "denied" || event.Site != "demo" || event.Actor.Email != "alice@example.com" {
		t.Fatalf("audit event = %+v", event)
	}
	if event.FileCount != 1 || event.TotalBytes == 0 {
		t.Fatalf("audit deploy size = %+v", event)
	}
}

func TestDeployStorageFailureIsAudited(t *testing.T) {
	auth := &recordingDeployAuth{action: "update"}
	srv := &Server{
		sites:          failingSiteStore{},
		deployAuth:     auth,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
	req := deployRequest(t, "spot.localhost", "demo", map[string]string{"index.html": "<h1>hi</h1>"})
	req.Header.Set("X-Forwarded-For", "100.64.0.7")

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("deploy with unreachable store = %d, want 500", rec.Code)
	}
	if len(auth.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(auth.events))
	}
	if event := auth.events[0]; event.Status != "failed" || event.Action != "update" {
		t.Fatalf("audit event = %+v", event)
	}
}

func TestDeployHandlesLocalPathShapeConflicts(t *testing.T) {
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:          sites,
		deployAuth:     &recordingDeployAuth{},
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	call := func(files map[string]string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "demo", files))
		return rec.Code
	}

	if code := call(map[string]string{"index.html": "home", "docs": "file"}); code != http.StatusOK {
		t.Fatalf("file deploy = %d, want 200", code)
	}
	if code := call(map[string]string{"index.html": "home", "docs/index.html": "dir"}); code != http.StatusOK {
		t.Fatalf("file-to-dir deploy = %d, want 200", code)
	}
	rc, _, err := sites.Open(context.Background(), "demo", "docs/index.html")
	if err != nil {
		t.Fatalf("open docs/index.html: %v", err)
	}
	rc.Close()
	if code := call(map[string]string{"index.html": "home", "docs": "file again"}); code != http.StatusOK {
		t.Fatalf("dir-to-file deploy = %d, want 200", code)
	}
	rc, _, err = sites.Open(context.Background(), "demo", "docs")
	if err != nil {
		t.Fatalf("open docs: %v", err)
	}
	rc.Close()
}

func TestUpdatePolicyCacheFromDeploy(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", accessFileName, `{"allow":["alice@example.com"],"ai":"visitors"}`)
	store := NewPolicyStore(dir, time.Hour)
	srv := &Server{policies: store}

	store.Set("demo", nil, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{
		{path: "index.html", data: []byte("<h1>hi</h1>")},
		{path: accessFileName, data: []byte(`{"allow":["alice@example.com"],"ai":"visitors"}`)},
	})
	policy, err := store.For("demo")
	if err != nil || policy == nil {
		t.Fatalf("cached policy = %v, %v; want policy", policy, err)
	}
	if !policy.Allows(Identity{Email: "alice@example.com"}) || policy.AllowsAIVisitors() {
		t.Fatalf("cached policy = %+v; want alice without early AI visitor opt-in", policy)
	}

	store.Set("demo", nil, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{
		{path: accessFileName, data: []byte(`{"download":false}`)},
	})
	policy, err = store.For("demo")
	if err != nil || policy == nil || policy.RestrictsAccess() || policy.AllowsDownload() {
		t.Fatalf("download opt-out cache = %+v, %v; want public site with downloads disabled", policy, err)
	}

	store.Set("demo", policy, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{
		{path: accessFileName, data: []byte(`{"allow":["alice@example.com"],"download":false}`)},
	})
	policy, err = store.For("demo")
	if err != nil || policy == nil || !policy.RestrictsAccess() || !policy.Allows(Identity{Email: "alice@example.com"}) || policy.AllowsDownload() {
		t.Fatalf("public download opt-out to restricted cache = %+v, %v; want restricted alice with downloads disabled", policy, err)
	}

	store.Set("demo", &AccessPolicy{Allow: []string{"alice@example.com"}}, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{
		{path: accessFileName, data: []byte(`{"allow":["alice@example.com","bob@example.com"]}`)},
	})
	policy, err = store.For("demo")
	if err != nil || policy == nil || !policy.Allows(Identity{Email: "alice@example.com"}) || policy.Allows(Identity{Email: "bob@example.com"}) {
		t.Fatalf("broadened policy before mount catches up = %+v, %v; want old alice-only policy", policy, err)
	}

	store.Set("demo", &AccessPolicy{Allow: []string{"alice@example.com"}}, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{
		{path: accessFileName, data: []byte(`{"download":false}`)},
	})
	policy, err = store.For("demo")
	if err != nil || policy == nil || !policy.RestrictsAccess() || !policy.Allows(Identity{Email: "alice@example.com"}) {
		t.Fatalf("restricted-to-public before mount catches up = %+v, %v; want old restricted policy", policy, err)
	}

	store.Set("demo", &AccessPolicy{Allow: []string{"alice@example.com", "bob@example.com"}, AI: aiAccessVisitors}, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{
		{path: accessFileName, data: []byte(`{"allow":["alice@example.com"]}`)},
	})
	policy, err = store.For("demo")
	if err != nil || policy == nil || !policy.Allows(Identity{Email: "alice@example.com"}) || policy.Allows(Identity{Email: "bob@example.com"}) || policy.AllowsAIVisitors() {
		t.Fatalf("narrowed policy = %+v, %v; want alice-only without AI visitors", policy, err)
	}

	store.Set("demo", &AccessPolicy{Allow: []string{"alice@example.com"}}, nil)
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{{path: "index.html", data: []byte("open")}})
	policy, err = store.For("demo")
	if err != nil || policy == nil || !policy.Allows(Identity{Email: "alice@example.com"}) {
		t.Fatalf("open policy before mount catches up = %v, %v; want old mounted policy", policy, err)
	}

	if err := os.Remove(filepath.Join(dir, "demo", accessFileName)); err != nil {
		t.Fatal(err)
	}
	srv.updatePolicyCacheFromDeploy("demo", []deployFile{{path: "index.html", data: []byte("open")}})
	policy, err = store.For("demo")
	if err != nil || policy != nil {
		t.Fatalf("policy after mounted access file removal = %v, %v; want open", policy, err)
	}

	srv.updatePolicyCacheFromDeploy("demo", []deployFile{{path: accessFileName, data: []byte(`{not json`)}})
	if _, err = store.For("demo"); err == nil {
		t.Fatal("cached invalid policy: want fail-closed parse error")
	}
}

func TestSiteRecordOwnershipPrefersEmail(t *testing.T) {
	record := SiteRecord{OwnerEmail: "owner@example.com", OwnerPeerIP: "100.64.0.7"}
	if record.OwnedBy(Identity{Email: "intruder@example.com", PeerIP: "100.64.0.7"}) {
		t.Fatal("same peer IP must not satisfy ownership when the site has an owner email")
	}
	if !record.OwnedBy(Identity{Email: "Owner@Example.com", PeerIP: "100.64.0.99"}) {
		t.Fatal("owner email should match case-insensitively")
	}

	peerOnly := SiteRecord{OwnerPeerIP: "100.64.0.7"}
	if !peerOnly.OwnedBy(Identity{PeerIP: "100.64.0.7"}) {
		t.Fatal("peer IP should own sites claimed without an email")
	}
}

// TestPartFilenameKeepsDirectories pins the reason partFilename exists:
// Part.FileName() strips directories, deploys must not.
func TestPartFilenameKeepsDirectories(t *testing.T) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="files"; filename="css/deep/app.css"`)
	if _, err := writer.CreatePart(header); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	reader := multipart.NewReader(&form, writer.Boundary())
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := partFilename(part); got != "css/deep/app.css" {
		t.Errorf("partFilename = %q, want css/deep/app.css", got)
	}
}
