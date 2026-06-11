package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Server struct {
	store       *DocStore
	resolver    *NetbirdResolver
	policies    *PolicyStore
	hub         *Hub
	files       *FileStore
	ai          *AIProxy
	maxUpload   int64
	quickDomain string

	dbLimit   *RateLimiter
	fileLimit *RateLimiter
	aiLimit   *RateLimiter
}

// requestHost is the host the browser addressed. Caddy overwrites
// X-Forwarded-Host on every proxied request (clients could otherwise
// set it themselves), so when present it is trustworthy — and it is the
// only host available on forward_auth subrequests, where r.Host is the
// backend's own address.
func requestHost(r *http.Request) string {
	if host := r.Header.Get("X-Forwarded-Host"); host != "" {
		return host
	}
	return r.Host
}

func (s *Server) routes() *http.ServeMux {
	// Lazy defaults keep tests terse; production wiring overrides these
	// in main. /api/authz is deliberately unlimited — Caddy consults it
	// for every static file request.
	if s.dbLimit == nil {
		s.dbLimit = NewRateLimiter(25, 50)
	}
	if s.fileLimit == nil {
		s.fileLimit = NewRateLimiter(2, 10)
	}
	if s.aiLimit == nil {
		s.aiLimit = NewRateLimiter(0.5, 10)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/authz", s.handleAuthz)
	mux.HandleFunc("GET /api/ws", s.handleWS)
	mux.HandleFunc("GET /api/db/{collection}", limited(s.dbLimit, s.handleList))
	mux.HandleFunc("POST /api/db/{collection}", limited(s.dbLimit, s.handleCreate))
	mux.HandleFunc("GET /api/db/{collection}/{id}", limited(s.dbLimit, s.handleGet))
	mux.HandleFunc("PUT /api/db/{collection}/{id}", limited(s.dbLimit, s.handleUpdate))
	mux.HandleFunc("DELETE /api/db/{collection}/{id}", limited(s.dbLimit, s.handleDelete))
	mux.HandleFunc("POST /api/files", limited(s.fileLimit, s.handleUpload))
	mux.HandleFunc("GET /api/files/{site}/{id}/{name}", s.handleDownload)
	mux.HandleFunc("POST /api/ai/chat", limited(s.aiLimit, s.handleAIChat))
	return mux
}

const defaultMaxUpload = 25 << 20

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		httpError(w, http.StatusServiceUnavailable,
			"file store not configured: set QUICK_S3_ENDPOINT and credentials")
		return
	}
	site := siteFromHost(requestHost(r), s.quickDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "files API must be called from a site subdomain")
		return
	}
	maxUpload := s.maxUpload
	if maxUpload == 0 {
		maxUpload = defaultMaxUpload
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds the %d MB limit", maxUpload>>20))
			return
		}
		httpError(w, http.StatusBadRequest, `multipart form field "file" is required`)
		return
	}
	defer file.Close()

	stored, err := s.files.Put(r.Context(), site, header.Filename,
		header.Header.Get("Content-Type"), file, header.Size)
	if err != nil {
		log.Printf("files: %v", err)
		httpError(w, http.StatusInternalServerError, "could not store the file")
		return
	}
	writeJSON(w, http.StatusCreated, stored)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		httpError(w, http.StatusServiceUnavailable,
			"file store not configured: set QUICK_S3_ENDPOINT and credentials")
		return
	}
	site, id, name := r.PathValue("site"), r.PathValue("id"), r.PathValue("name")
	obj, contentType, err := s.files.Get(r.Context(), site, id, name)
	if errors.Is(err, ErrNotFound) {
		httpError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		log.Printf("files: %v", err)
		httpError(w, http.StatusInternalServerError, "could not read the file")
		return
	}
	defer obj.Close()

	w.Header().Set("Content-Type", contentType)
	// IDs are random per upload, so content at a URL never changes.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if _, err := io.Copy(w, obj); err != nil {
		log.Printf("files: stream %s/%s/%s: %v", site, id, name, err)
	}
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

// handleAuthz answers Caddy's forward_auth subrequest for every site
// request. Sites without an access policy are open to everyone on the
// mesh; sites with one fail CLOSED whenever the policy or the visitor's
// identity cannot be established.
func (s *Server) handleAuthz(w http.ResponseWriter, r *http.Request) {
	site := siteFromHost(requestHost(r), s.quickDomain)
	if site == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	policy, err := s.policies.For(site)
	if err != nil {
		log.Printf("authz: %v", err)
		httpError(w, http.StatusServiceUnavailable,
			"this site's "+accessFileName+" is unreadable; access denied until it is fixed")
		return
	}
	if policy == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.resolver == nil {
		httpError(w, http.StatusServiceUnavailable,
			"site is restricted but the identity resolver is not configured")
		return
	}
	ip := clientIP(r)
	id, found, err := s.resolver.Resolve(r.Context(), ip)
	if err != nil {
		log.Printf("authz: resolve %s: %v", ip, err)
		httpError(w, http.StatusServiceUnavailable, "could not verify identity with NetBird")
		return
	}
	if !found || !policy.Allows(id) {
		httpError(w, http.StatusForbidden,
			"this site is restricted by its "+accessFileName+"; redeploy with your email or group to get in")
		return
	}
	w.WriteHeader(http.StatusOK)
}

type wsRequest struct {
	Type       string `json:"type"`
	Collection string `json:"collection"`
}

// handleWS serves realtime subscriptions. A session subscribes to
// collections with {"type":"subscribe","collection":"posts"} and
// receives Event messages; scoping follows the same rules as the
// database API (site-private, shared-* global).
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	site := siteFromHost(requestHost(r), s.quickDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "websocket API must be called from a site subdomain")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Proxied requests have Host quick-api:8080 while the browser's
		// Origin is the site, so the default same-host check would
		// reject everything. Any *.quickDomain origin is legitimate.
		OriginPatterns: []string{"*." + s.quickDomain, "*." + s.quickDomain + ":*"},
	})
	if err != nil {
		return // Accept has already written the error response
	}
	defer conn.CloseNow()

	ctx := r.Context()
	out := make(chan Event, 64)
	defer s.hub.UnsubscribeAll(out)

	reqs := make(chan wsRequest)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var req wsRequest
			if err := wsjson.Read(ctx, conn, &req); err != nil {
				return
			}
			select {
			case reqs <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case req := <-reqs:
			scope, err := scopeFor(site, req.Collection)
			if err != nil {
				_ = wsjson.Write(ctx, conn, map[string]string{"type": "error", "error": err.Error()})
				continue
			}
			switch req.Type {
			case "subscribe":
				s.hub.Subscribe(scope, req.Collection, out)
			case "unsubscribe":
				s.hub.Unsubscribe(scope, req.Collection, out)
			default:
				_ = wsjson.Write(ctx, conn, map[string]string{"type": "error", "error": "unknown request type " + req.Type})
			}
		case ev := <-out:
			if err := wsjson.Write(ctx, conn, ev); err != nil {
				return
			}
		}
	}
}

// scope resolves the request to a database namespace, writing the error
// response itself when the request is not a valid site request.
func (s *Server) scope(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	collection := r.PathValue("collection")
	site := siteFromHost(requestHost(r), s.quickDomain)
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
