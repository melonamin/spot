package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Server struct {
	store          *DocStore
	resolver       IdentityResolver
	policies       *PolicyStore
	hub            *Hub
	files          *FileStore
	sites          *SiteStore
	deployAuth     DeployAuthorizer
	ai             *AIProxy
	maxUpload      int64
	spotDomain     string
	trustedProxies *TrustedProxies

	dbLimit     *RateLimiter
	fileLimit   *RateLimiter
	aiLimit     *RateLimiter
	deployLimit *RateLimiter
}

// requestHost is the host the browser addressed. Caddy overwrites
// X-Forwarded-Host on every proxied request; it is only trustworthy when
// the socket peer is one of the configured front proxies.
func (s *Server) requestHost(r *http.Request) string {
	if s.trustsRemote(r) {
		if host := r.Header.Get("X-Forwarded-Host"); host != "" {
			return host
		}
	}
	return r.Host
}

func (s *Server) trustsRemote(r *http.Request) bool {
	trusted := s.trustedProxies
	if trusted == nil {
		trusted = defaultTrustedProxies
	}
	return trusted.ContainsRemote(r.RemoteAddr)
}

func (s *Server) rejectUntrustedForwardedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.trustsRemote(r) && hasForwardedHeaders(r) {
			httpError(w, http.StatusBadRequest, "forwarded headers are only accepted from trusted proxies")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasForwardedHeaders(r *http.Request) bool {
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host"} {
		if r.Header.Get(header) != "" {
			return true
		}
	}
	return false
}

func (s *Server) routes() http.Handler {
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
	if s.deployLimit == nil {
		s.deployLimit = NewRateLimiter(0.5, 3)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/me", s.sameOriginOnly(s.handleMe))
	mux.HandleFunc("GET /api/access/suggestions", s.sameOriginOnly(s.limited(s.dbLimit, s.handleAccessSuggestions)))
	mux.HandleFunc("GET /api/authz", s.handleAuthz)
	mux.HandleFunc("GET /api/ws", s.handleWS)
	mux.HandleFunc("GET /api/db/{collection}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleList)))
	mux.HandleFunc("POST /api/db/{collection}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleCreate)))
	mux.HandleFunc("GET /api/db/{collection}/{id}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleGet)))
	mux.HandleFunc("PUT /api/db/{collection}/{id}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleUpdate)))
	mux.HandleFunc("DELETE /api/db/{collection}/{id}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleDelete)))
	mux.HandleFunc("POST /api/deploy", s.sameOriginOnly(s.limited(s.deployLimit, s.handleDeploy)))
	mux.HandleFunc("POST /api/files", s.sameOriginOnly(s.limited(s.fileLimit, s.handleUpload)))
	mux.HandleFunc("GET /api/files/{site}/{id}/{name}", s.handleDownload)
	mux.HandleFunc("POST /api/ai/chat", s.sameOriginOnly(s.limited(s.aiLimit, s.handleAIChat)))
	return s.rejectUntrustedForwardedHeaders(mux)
}

// sameOriginOnly rejects browser-originated cross-site API calls. Spot
// identity is ambient mesh identity, not a per-site CSRF token, so a
// site must not be able to spend a visitor's authorization on another
// site by targeting that site's /api paths.
func (s *Server) sameOriginOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.originMatchesHost(r) {
			httpError(w, http.StatusForbidden, "cross-site API requests are not allowed")
			return
		}
		next(w, r)
	}
}

func (s *Server) originMatchesHost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return sameHost(u.Host, s.requestHost(r))
}

func (s *Server) limited(l *RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(s.clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			httpError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
			return
		}
		next(w, r)
	}
}

func (s *Server) resolveIdentity(w http.ResponseWriter, r *http.Request, purpose string) (Identity, bool) {
	if s.resolver == nil {
		httpError(w, http.StatusServiceUnavailable,
			"identity resolver not configured: set NETBIRD_API_URL/NETBIRD_API_TOKEN or explicit dev identity")
		return Identity{}, false
	}
	ip := s.clientIP(r)
	id, found, err := s.resolver.Resolve(r.Context(), ip)
	if err != nil {
		log.Printf("%s: resolve %s: %v", purpose, ip, err)
		httpError(w, http.StatusBadGateway, "could not reach the identity resolver")
		return Identity{}, false
	}
	if !found {
		httpError(w, http.StatusNotFound, "no identity matches "+ip)
		return Identity{}, false
	}
	if id.PeerIP == "" {
		id.PeerIP = ip
	}
	return id, true
}

func (s *Server) authorizeSiteAccess(w http.ResponseWriter, r *http.Request, site string) bool {
	if site == "" || s.policies == nil {
		return true
	}
	policy, err := s.policies.For(site)
	if err != nil {
		log.Printf("authz: %v", err)
		httpError(w, http.StatusServiceUnavailable,
			"this site's "+accessFileName+" is unreadable; access denied until it is fixed")
		return false
	}
	if policy == nil {
		return true
	}
	if s.resolver == nil {
		httpError(w, http.StatusServiceUnavailable,
			"site is restricted but the identity resolver is not configured")
		return false
	}
	ip := s.clientIP(r)
	id, found, err := s.resolver.Resolve(r.Context(), ip)
	if err != nil {
		log.Printf("authz: resolve %s: %v", ip, err)
		httpError(w, http.StatusServiceUnavailable, "could not verify identity with NetBird")
		return false
	}
	if id.PeerIP == "" {
		id.PeerIP = ip
	}
	if !found || !policy.Allows(id) {
		httpError(w, http.StatusForbidden,
			"this site is restricted by its "+accessFileName)
		return false
	}
	return true
}

func (s *Server) requireDeployIdentity(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, ok := s.resolveIdentity(w, r, "deploy")
	if !ok {
		return Identity{}, false
	}
	if actorKey(id) == "" {
		httpError(w, http.StatusForbidden, "deploy requires an identified NetBird user or peer")
		return Identity{}, false
	}
	return id, true
}

func (s *Server) recordDeployAudit(r *http.Request, event DeployAuditEvent) {
	if s.deployAuth == nil {
		return
	}
	if err := s.deployAuth.RecordDeploy(r.Context(), event); err != nil {
		log.Printf("deploy audit %s: %v", event.Site, err)
	}
}

func sameHost(a, b string) bool {
	return strings.EqualFold(normalizeHost(a), normalizeHost(b))
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

const defaultMaxUpload = 25 << 20

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		httpError(w, http.StatusServiceUnavailable,
			"file store not configured: set SPOT_S3_ENDPOINT and credentials")
		return
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "files API must be called from a site subdomain")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
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

	// Sniff the content type from the bytes rather than trusting the
	// client's declared type — that header controls how a browser renders
	// the download, so a forged image/png on HTML bytes would be a stored
	// XSS in the viewer's site origin.
	sniff := make([]byte, 512)
	n, _ := io.ReadFull(file, sniff)
	contentType := http.DetectContentType(sniff[:n])
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		log.Printf("files: seek: %v", err)
		httpError(w, http.StatusInternalServerError, "could not read the upload")
		return
	}

	stored, err := s.files.Put(r.Context(), site, header.Filename, contentType, file, header.Size)
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
			"file store not configured: set SPOT_S3_ENDPOINT and credentials")
		return
	}
	site, id, name := r.PathValue("site"), r.PathValue("id"), r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid file site")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
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
	// Defense in depth against a rendered upload running script in a
	// viewer's site origin: never let the browser re-sniff, sandbox the
	// response, and only allow inline rendering for known-safe media —
	// everything else (HTML, SVG, unknown) downloads instead of renders.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	disposition := "attachment"
	if inlineSafe(contentType) {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	// IDs are random per upload, so content at a URL never changes.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if _, err := io.Copy(w, obj); err != nil {
		log.Printf("files: stream %s/%s/%s: %v", site, id, name, err)
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveIdentity(w, r, "identity")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, id)
}

const maxAccessSuggestions = 20

// handleAccessSuggestions powers the deployer's access picker: it
// searches the NetBird directory for users (by email) and groups (by
// name) matching ?q. Like /api/deploy it only answers on the apex — the
// picker lives on the platform page, and a deployed site must not be
// able to harvest the org directory through a visitor's identity.
func (s *Server) handleAccessSuggestions(w http.ResponseWriter, r *http.Request) {
	if siteFromHost(s.requestHost(r), s.spotDomain) != "" {
		httpError(w, http.StatusBadRequest,
			"the access directory is served on the platform root, not on site subdomains")
		return
	}
	if _, ok := s.resolveIdentity(w, r, "access"); !ok {
		return
	}
	out := make([]AccessSuggestion, 0, maxAccessSuggestions)
	dir, ok := s.resolver.(DirectoryResolver)
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if ok && query != "" {
		suggestions, err := dir.Directory(r.Context())
		if err != nil {
			log.Printf("access suggestions: %v", err)
			httpError(w, http.StatusBadGateway, "could not reach the identity directory")
			return
		}
		for _, sug := range suggestions {
			if strings.Contains(strings.ToLower(sug.Label), query) ||
				strings.Contains(strings.ToLower(sug.Meta), query) {
				out = append(out, sug)
				if len(out) >= maxAccessSuggestions {
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}

// handleAuthz answers Caddy's forward_auth subrequest for every site
// request. Sites without an access policy are open to everyone on the
// mesh; sites with one fail CLOSED whenever the policy or the visitor's
// identity cannot be established.
func (s *Server) handleAuthz(w http.ResponseWriter, r *http.Request) {
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.authorizeSiteAccess(w, r, site) {
		w.WriteHeader(http.StatusOK)
	}
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
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "websocket API must be called from a site subdomain")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
	acceptReq := r.Clone(r.Context())
	acceptReq.Host = s.requestHost(r)
	conn, err := websocket.Accept(w, acceptReq, nil)
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
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if !s.authorizeSiteAccess(w, r, site) {
		return "", "", false
	}
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
