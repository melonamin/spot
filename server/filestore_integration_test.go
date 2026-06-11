//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestFileUploadRoundtrip exercises the upload and download handlers
// against the real RustFS from the compose stack (`just up` first).
func TestFileUploadRoundtrip(t *testing.T) {
	endpoint := os.Getenv("QUICK_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	files, err := NewFileStore(endpoint, "rustfsadmin", "rustfsadmin", "quick-uploads")
	if err != nil {
		t.Fatalf("file store: %v", err)
	}
	srv := &Server{files: files, quickDomain: "quick.localhost"}
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
	req.Header.Set("X-Forwarded-Host", "it-files.quick.localhost")
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
