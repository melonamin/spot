package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
)

var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Server struct {
	store       *DocStore
	resolver    *NetbirdResolver
	quickDomain string
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/db/{collection}", s.handleList)
	mux.HandleFunc("POST /api/db/{collection}", s.handleCreate)
	mux.HandleFunc("GET /api/db/{collection}/{id}", s.handleGet)
	mux.HandleFunc("PUT /api/db/{collection}/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE /api/db/{collection}/{id}", s.handleDelete)
	return mux
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if s.resolver == nil {
		httpError(w, http.StatusServiceUnavailable,
			"identity resolver not configured: set NETBIRD_API_URL and NETBIRD_API_TOKEN")
		return
	}
	ip := clientIP(r)
	id, found, err := s.resolver.Resolve(r.Context(), ip)
	if err != nil {
		log.Printf("identity: resolve %s: %v", ip, err)
		httpError(w, http.StatusBadGateway, "could not reach the NetBird API")
		return
	}
	if !found {
		httpError(w, http.StatusNotFound, "no NetBird peer matches "+ip)
		return
	}
	writeJSON(w, http.StatusOK, id)
}

// scope resolves the request to a database namespace, writing the error
// response itself when the request is not a valid site request.
func (s *Server) scope(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	collection := r.PathValue("collection")
	site := siteFromHost(r.Host, s.quickDomain)
	scope, err := scopeFor(site, collection)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return "", "", false
	}
	return scope, collection, true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			httpError(w, http.StatusBadRequest, "limit must be an integer between 1 and 1000")
			return
		}
		limit = n
	}
	docs, err := s.store.List(r.Context(), scope, collection, limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	data, ok := readDocument(w, r)
	if !ok {
		return
	}
	doc, err := s.store.Create(r.Context(), scope, collection, data)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	id, ok := readID(w, r)
	if !ok {
		return
	}
	doc, err := s.store.Get(r.Context(), scope, collection, id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	id, ok := readID(w, r)
	if !ok {
		return
	}
	data, ok := readDocument(w, r)
	if !ok {
		return
	}
	doc, err := s.store.Update(r.Context(), scope, collection, id, data)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	id, ok := readID(w, r)
	if !ok {
		return
	}
	if err := s.store.Delete(r.Context(), scope, collection, id); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		httpError(w, http.StatusNotFound, "document not found")
		return
	}
	log.Printf("docstore: %v", err)
	httpError(w, http.StatusInternalServerError, "database error")
}

func readID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, http.StatusBadRequest, "document id must be a UUID")
		return "", false
	}
	return id, true
}

func readDocument(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var data map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&data); err != nil {
		httpError(w, http.StatusBadRequest, "request body must be a JSON object")
		return nil, false
	}
	if data == nil {
		httpError(w, http.StatusBadRequest, "request body must be a JSON object, not null")
		return nil, false
	}
	return data, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func httpError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
