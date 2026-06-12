//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
)

// TestFileUploadRoundtrip exercises the upload and download handlers
// against the real RustFS from the compose stack (`just up` first).
func TestFileUploadRoundtrip(t *testing.T) {
	endpoint := os.Getenv("SPOT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	files, err := NewFileStore(endpoint, "rustfsadmin", "rustfsadmin", "spot-uploads")
	if err != nil {
		t.Fatalf("file store: %v", err)
	}
	srv := &Server{files: files, spotDomain: "spot.localhost"}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	content := []byte("integration test payload \x00\x01\x02")
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("file", "it test payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	writer.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/files", &form)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Forwarded-Host", "it-files.spot.localhost")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("upload (is `just up` running?): status %d body %s", res.StatusCode, body)
	}

	var stored StoredFile
	if err := json.NewDecoder(res.Body).Decode(&stored); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if stored.Name != "it_test_payload.bin" || stored.Size != int64(len(content)) {
		t.Errorf("stored = %+v", stored)
	}

	got, err := http.Get(ts.URL + stored.URL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d", got.StatusCode)
	}
	roundtripped, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundtripped, content) {
		t.Errorf("downloaded %d bytes do not match the %d uploaded", len(roundtripped), len(content))
	}

	missing, err := http.Get(ts.URL + "/api/files/it-files/00000000000000000000000000000000/nope.bin")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: status %d, want 404", missing.StatusCode)
	}
}

// TestFileUploadHTMLIsNotRenderable verifies the XSS defense: an HTML
// upload is sniffed (not trusted) and served as a sandboxed attachment
// so a browser never executes it in the viewer's site origin.
func TestFileUploadHTMLIsNotRenderable(t *testing.T) {
	endpoint := os.Getenv("SPOT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	files, err := NewFileStore(endpoint, "rustfsadmin", "rustfsadmin", "spot-uploads")
	if err != nil {
		t.Fatalf("file store: %v", err)
	}
	srv := &Server{files: files, spotDomain: "spot.localhost"}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	// Claim image/png, but send HTML bytes — the classic disguised upload.
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="evil.png"`)
	header.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("<html><script>alert(document.domain)</script></html>"))
	writer.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/files", &form)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Forwarded-Host", "it-files.spot.localhost")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status %d", res.StatusCode)
	}
	var stored StoredFile
	json.NewDecoder(res.Body).Decode(&stored)
	// The client claimed image/png; sniffing must override it to text/html.
	if stored.ContentType != "text/html; charset=utf-8" {
		t.Errorf("stored content type = %q, want sniffed text/html", stored.ContentType)
	}

	got, err := http.Get(ts.URL + stored.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if disp := got.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment") {
		t.Errorf("HTML upload Content-Disposition = %q, want attachment", disp)
	}
	if got.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
	if !strings.Contains(got.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("CSP = %q, want sandbox", got.Header.Get("Content-Security-Policy"))
	}
}
