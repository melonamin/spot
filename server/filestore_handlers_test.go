package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFileListAndDeleteHandlers exercises the new files endpoints end to
// end against a local store: upload, list, delete via the same path the
// SDK uses, and confirm the deleted upload is gone.
func TestFileListAndDeleteHandlers(t *testing.T) {
	files, err := NewLocalFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		files:      files,
		policies:   NewPolicyStore(t.TempDir(), time.Second),
		spotDomain: "spot.localhost",
	}

	upload := func(filename, content string) StoredFile {
		t.Helper()
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte(content))
		writer.Close()
		req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/files", &form)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: status %d body %s", filename, rec.Code, rec.Body)
		}
		var stored StoredFile
		if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
			t.Fatalf("decode upload: %v", err)
		}
		return stored
	}

	list := func() []StoredFile {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/files", nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: status %d body %s", rec.Code, rec.Body)
		}
		var out struct {
			Files []StoredFile `json:"files"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out.Files
	}

	a := upload("a.txt", "aaa")
	upload("b.txt", "bb")

	if got := list(); len(got) != 2 {
		t.Fatalf("list = %d files, want 2", len(got))
	}

	// Delete using the path shape the SDK derives from the stored URL.
	del := httptest.NewRequest(http.MethodDelete,
		"http://demo.spot.localhost/api/files/"+a.ID+"/"+a.Name, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, del)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body)
	}

	if got := list(); len(got) != 1 || got[0].Name != "b.txt" {
		t.Fatalf("list after delete = %+v, want only b.txt", got)
	}

	// Downloading the deleted upload now 404s.
	dl := httptest.NewRequest(http.MethodGet,
		"http://demo.spot.localhost/api/files/demo/"+a.ID+"/"+a.Name, nil)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, dl)
	if rec.Code != http.StatusNotFound {
		t.Errorf("download deleted = %d, want 404", rec.Code)
	}

	// A malformed id is rejected before the store is touched.
	badDel := httptest.NewRequest(http.MethodDelete,
		"http://demo.spot.localhost/api/files/not-an-id/x.txt", nil)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, badDel)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("delete with bad id = %d, want 400", rec.Code)
	}
}
