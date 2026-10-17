package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	typesAdminEmail = "admin@example.com"
	typesOwnerEmail = "owner@example.com"
)

// typesEnv is a full server over a real SQLite registry and local storage,
// with a switchable caller identity.
type typesEnv struct {
	t        *testing.T
	srv      *Server
	handler  http.Handler
	db       *sql.DB
	registry *SiteRegistry
	root     string
}

func newTypesEnv(t *testing.T) *typesEnv {
	t.Helper()
	db := openTestDB(t)
	admins := &AccessPolicy{Allow: []string{typesAdminEmail}}
	registry := NewSiteRegistry(db, admins)
	root := t.TempDir()
	sites, err := NewLocalSiteStore(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites: sites, policies: NewPolicyStore(root, time.Minute),
		siteAdmin: registry, siteManager: registry, deployAuth: registry,
		adminPolicy: admins, types: NewTypeRegistry(db), hub: NewHub(),
		spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t), serveStatic: true,
		dbLimit: NewRateLimiter(1000, 1000), deployLimit: NewRateLimiter(1000, 1000),
		fileLimit: NewRateLimiter(1000, 1000),
	}
	registry.SetPolicyResolver(srv.policyForSite)
	env := &typesEnv{t: t, srv: srv, db: db, registry: registry, root: root}
	env.as(typesOwnerEmail)
	env.handler = srv.routes()
	return env
}

func (e *typesEnv) as(email string) {
	e.srv.resolver = NewStaticResolver(email, strings.Split(email, "@")[0], nil)
}

func (e *typesEnv) do(req *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *typesEnv) get(host, target string, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return e.do(req)
}

func (e *typesEnv) jsonRequest(method, target string, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "http://spot.localhost"+target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return e.do(req)
}

// upload posts a package as the platform admin.
func (e *typesEnv) upload(files map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	previous := e.srv.resolver
	e.as(typesAdminEmail)
	defer func() { e.srv.resolver = previous }()
	return e.do(typeUploadRequest(e.t, files))
}

func (e *typesEnv) mustUpload(files map[string]string) {
	e.t.Helper()
	if rec := e.upload(files); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		e.t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}
}

func (e *typesEnv) deploy(site string, files map[string]string, fields map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var ordered [][2]string
	for p, content := range files {
		ordered = append(ordered, [2]string{p, content})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i][0] < ordered[j][0] })
	return e.do(deployRequestOrderedFields(e.t, "spot.localhost", site, ordered, fields))
}

func (e *typesEnv) mustDeploy(site string, files map[string]string, fields map[string]string) {
	e.t.Helper()
	if rec := e.deploy(site, files, fields); rec.Code != http.StatusOK {
		e.t.Fatalf("deploy %s = %d %s", site, rec.Code, rec.Body.String())
	}
}

func (e *typesEnv) siteType(site string) siteTypeRef {
	e.t.Helper()
	ref, err := e.srv.types.SiteType(e.t.Context(), site)
	if err != nil {
		e.t.Fatal(err)
	}
	return ref
}

func typeUploadRequest(t *testing.T, files map[string]string) *http.Request {
	t.Helper()
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="files"; filename="`+p+`"`)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(part, files[p])
	}
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "http://spot.localhost/api/types", &form)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// examplePackage reads examples/types/card, the package Spot's docs use.
func examplePackage(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join("..", "examples", "types", "card")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(data)
	}
	return files
}

// cardVersion is the example package relabelled with another version; the
// viewer entry carries the version so each one has distinct bytes.
func cardVersion(t *testing.T, version string) map[string]string {
	t.Helper()
	files := examplePackage(t)
	var manifest map[string]any
	if err := json.Unmarshal([]byte(files[typeManifestFile]), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["version"] = version
	data, _ := json.Marshal(manifest)
	files[typeManifestFile] = string(data)
	files[typeEntryFile] = strings.Replace(files[typeEntryFile], "<title>Card</title>", "<title>Card "+version+"</title>", 1)
	return files
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	code, _ := decodeJSON[map[string]any](t, rec)["code"].(string)
	return code
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
