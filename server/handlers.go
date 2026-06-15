package main

import (
	"context"
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
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var roomEventRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

type Server struct {
	store          *DocStore
	resolver       IdentityResolver
	forwardAuth    *ForwardAuth
	policies       *PolicyStore
	hub            *Hub
	roomHub        *RoomHub
	files          FileStorage
	sites          SiteStorage
	deployAuth     DeployAuthorizer
	siteAdmin      SiteAdmin
	siteManager    SiteManager
	ai             *AIProxy
	aiAccess       string
	maxUpload      int64
	spotDomain     string
	trustedProxies *TrustedProxies
	serveStatic    bool
	siteLocksMu    sync.Mutex
	siteLocks      map[string]*sync.Mutex

	dbLimit       *RateLimiter
	fileLimit     *RateLimiter
	aiLimit       *RateLimiter
	deployLimit   *RateLimiter
	realtimeLimit *RateLimiter
}

// requestHost is the host the browser addressed. Caddy overwrites
// X-Forwarded-Host on every proxied request; it is only trustworthy when
// the socket peer is one of the configured front proxies.
func (s *Server) requestHost(r *http.Request) string {
	if s.trustsRemote(r) {
		if vals := r.Header.Values("X-Forwarded-Host"); len(vals) > 0 {
			if host := strings.TrimSpace(vals[len(vals)-1]); host != "" {
				return host
			}
		}
	}
	return r.Host
}

func (s *Server) requestScheme(r *http.Request) string {
	if s.trustsRemote(r) {
		if vals := r.Header.Values("X-Forwarded-Proto"); len(vals) > 0 {
			if proto := lastForwardedProto(vals[len(vals)-1]); proto != "" {
				return proto
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func lastForwardedProto(raw string) string {
	entries := strings.Split(raw, ",")
	proto := strings.ToLower(strings.TrimSpace(entries[len(entries)-1]))
	if proto == "http" || proto == "https" {
		return proto
	}
	return ""
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
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
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
	if s.realtimeLimit == nil {
		s.realtimeLimit = NewRateLimiter(30, 60)
	}
	if s.hub == nil {
		s.hub = NewHub()
	}
	if s.roomHub == nil {
		s.roomHub = NewRoomHub()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
	mux.HandleFunc("GET /api/me", s.sameOriginOnly(s.handleMe))
	mux.HandleFunc("GET /api/access/suggestions", s.sameOriginOnly(s.limited(s.dbLimit, s.handleAccessSuggestions)))
	mux.HandleFunc("GET /api/authz", s.handleAuthz)
	mux.HandleFunc("GET /api/ws", s.limited(s.dbLimit, s.handleWS))
	mux.HandleFunc("GET /api/db/{collection}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleList)))
	mux.HandleFunc("GET /api/db/{collection}/count", s.sameOriginOnly(s.limited(s.dbLimit, s.handleCount)))
	mux.HandleFunc("POST /api/db/{collection}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleCreate)))
	mux.HandleFunc("GET /api/db/{collection}/{id}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleGet)))
	mux.HandleFunc("PUT /api/db/{collection}/{id}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleUpdate)))
	mux.HandleFunc("POST /api/db/{collection}/{id}/increment", s.sameOriginOnly(s.limited(s.dbLimit, s.handleIncrement)))
	mux.HandleFunc("DELETE /api/db/{collection}/{id}", s.sameOriginOnly(s.limited(s.dbLimit, s.handleDelete)))
	mux.HandleFunc("POST /api/deploy", s.sameOriginOnly(s.limited(s.deployLimit, s.handleDeploy)))
	mux.HandleFunc("GET /api/download", s.limited(s.fileLimit, s.handleSiteDownload))
	mux.HandleFunc("GET /api/sites/mine", s.sameOriginOnly(s.limited(s.dbLimit, s.handleMySites)))
	mux.HandleFunc("GET /api/sites/public", s.sameOriginOnly(s.limited(s.dbLimit, s.handlePublicSites)))
	mux.HandleFunc("GET /api/sites/{name}/preview", s.handleSitePreview)
	mux.HandleFunc("DELETE /api/sites/{name}", s.sameOriginOnly(s.limited(s.deployLimit, s.handleDeleteSite)))
	mux.HandleFunc("GET /api/files", s.sameOriginOnly(s.limited(s.fileLimit, s.handleFileList)))
	mux.HandleFunc("POST /api/files", s.sameOriginOnly(s.limited(s.fileLimit, s.handleUpload)))
	mux.HandleFunc("GET /api/files/{site}/{id}/{name}", s.handleDownload)
	mux.HandleFunc("DELETE /api/files/{id}/{name}", s.sameOriginOnly(s.limited(s.fileLimit, s.handleFileDelete)))
	mux.HandleFunc("POST /api/ai/chat", s.sameOriginOnly(s.limited(s.aiLimit, s.handleAIChat)))
	mux.HandleFunc("POST /api/ai/chat/stream", s.sameOriginOnly(s.limited(s.aiLimit, s.handleAIChatStream)))
	mux.HandleFunc("POST /api/ai/image", s.sameOriginOnly(s.limited(s.aiLimit, s.handleAIImage)))
	mux.HandleFunc("/api/", http.NotFound)
	mux.HandleFunc("/api", http.NotFound)
	if s.serveStatic {
		mux.HandleFunc("/", s.handleStatic)
	}
	return s.rejectUntrustedForwardedHeaders(s.rejectUnknownHosts(mux))
}

func (s *Server) rejectUnknownHosts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || validSpotHost(s.requestHost(r), s.spotDomain) {
			next.ServeHTTP(w, r)
			return
		}
		httpError(w, http.StatusBadRequest, "request host is not part of this Spot domain")
	})
}

func (s *Server) siteMutationLock(site string) *sync.Mutex {
	s.siteLocksMu.Lock()
	defer s.siteLocksMu.Unlock()
	if s.siteLocks == nil {
		s.siteLocks = make(map[string]*sync.Mutex)
	}
	lock := s.siteLocks[site]
	if lock == nil {
		lock = &sync.Mutex{}
		s.siteLocks[site] = lock
	}
	return lock
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
	if !strings.EqualFold(u.Scheme, s.requestScheme(r)) {
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

// resolvePeer looks up the visitor's mesh identity by client IP, filling
// PeerIP from the request when the resolver omits it. It returns found=false
// with a nil error when no resolver is configured or the peer is not in the
// mesh, and a non-nil error for a resolver outage. It writes no response, so
// callers map the outcome to their own status. Shared by resolveIdentity and
// callerKey so the lookup cannot drift between them.
// forwardAuthIdentity returns the identity asserted by a trusted auth proxy
// via forward-auth headers. It is only honored when forward auth is enabled
// and the socket peer is a trusted proxy, so an untrusted client cannot
// assert an identity by sending Remote-* headers. ok is false when no proxy
// identity applies, so callers fall through to the mesh resolver.
func (s *Server) forwardAuthIdentity(r *http.Request) (Identity, bool) {
	if s.forwardAuth == nil || !s.trustsRemote(r) {
		return Identity{}, false
	}
	return s.forwardAuth.identityFrom(r.Header, s.clientIP(r))
}

func (s *Server) resolvePeer(r *http.Request) (Identity, bool, error) {
	if id, ok := s.forwardAuthIdentity(r); ok {
		return id, true, nil
	}
	if s.resolver == nil {
		return Identity{}, false, nil
	}
	ip := s.clientIP(r)
	id, found, err := s.resolver.Resolve(r.Context(), ip)
	if err != nil {
		return Identity{}, false, err
	}
	if !found {
		return Identity{}, false, nil
	}
	if id.PeerIP == "" {
		id.PeerIP = ip
	}
	return id, true, nil
}

func (s *Server) resolveIdentity(w http.ResponseWriter, r *http.Request, purpose string) (Identity, bool) {
	if s.resolver == nil && s.forwardAuth == nil {
		httpError(w, http.StatusServiceUnavailable,
			"identity resolver not configured: set SPOT_AUTH_MODE=single-user, NETBIRD_API_URL/NETBIRD_API_TOKEN, TAILSCALE_API_TOKEN, TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET, SPOT_FORWARD_AUTH, or explicit dev identity")
		return Identity{}, false
	}
	id, found, err := s.resolvePeer(r)
	if err != nil {
		log.Printf("%s: resolve %s: %v", purpose, s.clientIP(r), err)
		httpError(w, http.StatusBadGateway, "could not reach the identity resolver")
		return Identity{}, false
	}
	if !found {
		httpError(w, http.StatusNotFound, "no identity matches "+s.clientIP(r))
		return Identity{}, false
	}
	if id.Groups == nil {
		id.Groups = []string{}
	}
	return id, true
}

// callerKey resolves the visitor's stable identity key (lowercased email
// or peer IP, see actorKey) without writing any error response. It returns
// "" with a nil error when no resolver is configured or the peer is not in
// the mesh, so document ownership is best-effort on open, resolver-less
// installs. A non-nil error means the resolver itself failed (a transient
// outage): a mine-scoped read or write surfaces it as 503, while the create
// path treats it as best-effort and stamps an empty owner so an open site
// keeps accepting writes during a blip.
func (s *Server) callerKey(r *http.Request) (string, error) {
	id, found, err := s.resolvePeer(r)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return actorKey(id), nil
}

func (s *Server) authorizeSiteAccess(w http.ResponseWriter, r *http.Request, site string) bool {
	if site == "" {
		return true
	}
	policy, err := s.policyForSite(r.Context(), site)
	if err != nil {
		log.Printf("authz: %v", err)
		httpError(w, http.StatusServiceUnavailable,
			"this site's "+accessFileName+" is unreadable; access denied until it is fixed")
		return false
	}
	if policy == nil || !policy.RestrictsAccess() {
		return true
	}
	if s.resolver == nil && s.forwardAuth == nil {
		httpError(w, http.StatusServiceUnavailable,
			"site is restricted but the identity resolver is not configured")
		return false
	}
	id, found, err := s.resolvePeer(r)
	if err != nil {
		log.Printf("authz: resolve %s: %v", s.clientIP(r), err)
		httpError(w, http.StatusServiceUnavailable, "could not verify identity with the mesh provider")
		return false
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
		httpError(w, http.StatusForbidden, "deploy requires an identified mesh user or peer")
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

// requireFileSite runs the shared preamble for the host-addressed file
// handlers: it checks the file store is configured, resolves the site from the
// request host, and authorizes site access. It writes the matching error
// response and returns ok=false on failure. handleDownload takes the site from
// the path instead, so it does not use this.
func (s *Server) requireFileSite(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.files == nil {
		httpError(w, http.StatusServiceUnavailable,
			"file store not configured: set SPOT_S3_ENDPOINT and credentials")
		return "", false
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "files API must be called from a site subdomain")
		return "", false
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return "", false
	}
	return site, true
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requireFileSite(w, r)
	if !ok {
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

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requireFileSite(w, r)
	if !ok {
		return
	}
	files, err := s.files.List(r.Context(), site)
	if err != nil {
		log.Printf("files: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list files")
		return
	}
	if files == nil {
		files = []StoredFile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	site, ok := s.requireFileSite(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !fileIDRe.MatchString(id) {
		httpError(w, http.StatusBadRequest, "invalid file id")
		return
	}
	if err := s.files.Delete(r.Context(), site, id, r.PathValue("name")); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpError(w, http.StatusNotFound, "file not found")
			return
		}
		log.Printf("files: %v", err)
		httpError(w, http.StatusInternalServerError, "could not delete the file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// meResponse is the identity plus capabilities a page can use to gate its UI.
// Identity is embedded so its fields (email, name, peer_name, peer_ip, groups)
// stay at the top level.
type meResponse struct {
	Identity
	AIAllowed bool `json:"ai_allowed"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveIdentity(w, r, "identity")
	if !ok {
		return
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	writeJSON(w, http.StatusOK, meResponse{
		Identity:  id,
		AIAllowed: s.aiAllowedFor(r.Context(), site, id),
	})
}

// aiAllowedFor reports whether the visitor may spend the deployment's
// server-side AI key on this site, mirroring authorizeAIUse without writing a
// response. It is a best-effort read for UI gating: any inability to determine
// access (apex root, unreadable policy, unconfigured owner checks, lookup
// error) reports false rather than failing the request. It shares the visitor
// rule (aiVisitorsAllowed) with authorizeAIUse; keep the two in step.
func (s *Server) aiAllowedFor(ctx context.Context, site string, actor Identity) bool {
	if site == "" || s.ai == nil || !s.ai.configured() {
		return false
	}
	policy, err := s.policyForSite(ctx, site)
	if err != nil {
		return false
	}
	if policy != nil && policy.RestrictsAccess() && !policy.Allows(actor) {
		return false
	}
	if s.aiVisitorsAllowed(policy) {
		return true
	}
	if s.siteManager == nil {
		return false
	}
	// Only an owner-gated site that does not opt visitors in reaches here, so
	// the ownership lookup runs solely when it is the only way to answer; the
	// cheaper visitor/policy paths above short-circuit the common cases. This
	// is one indexed sites-table read per /api/me call in the default owners
	// mode — acceptable for a per-page-load capability check, so it is not cached.
	allowed, err := s.siteManager.CanManageSite(ctx, site, actor)
	return err == nil && allowed
}

const maxAccessSuggestions = 20

// handleAccessSuggestions powers the deployer's access picker: it
// searches the mesh directory for users (by email) and groups (by
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
	Type       string          `json:"type"`
	Collection string          `json:"collection"`
	Room       string          `json:"room"`
	Event      string          `json:"event"`
	Data       json.RawMessage `json:"data"`
}

const maxRealtimeData = 16 << 10

// handleWS serves realtime subscriptions. A session subscribes to
// collections with {"type":"subscribe","collection":"posts"} and joins
// ephemeral rooms with {"type":"room_join","room":"control"}. Scoping
// follows the same rules as the database API: site-private by default,
// shared-* global.
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
	conn.SetReadLimit(maxRealtimeData + 2048)

	ctx := r.Context()
	sessionID, err := newSessionID()
	if err != nil {
		log.Printf("websocket: %v", err)
		_ = conn.Close(websocket.StatusInternalError, "could not start realtime session")
		return
	}
	docOut := make(chan Event, 64)
	roomOut := make(chan RoomEvent, 64)
	defer s.hub.UnsubscribeAll(docOut)
	defer s.roomHub.LeaveAll(sessionID)

	var roomIdentity *Identity

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
			switch req.Type {
			case "subscribe":
				scope, err := scopeFor(site, req.Collection)
				if err != nil {
					writeWSError(ctx, conn, err.Error())
					continue
				}
				s.hub.Subscribe(scope, req.Collection, docOut)
				if err := wsjson.Write(ctx, conn, map[string]string{
					"type":       "subscribed",
					"collection": req.Collection,
				}); err != nil {
					return
				}
			case "unsubscribe":
				scope, err := scopeFor(site, req.Collection)
				if err != nil {
					writeWSError(ctx, conn, err.Error())
					continue
				}
				s.hub.Unsubscribe(scope, req.Collection, docOut)
			case "room_join":
				scope, ok := s.roomRequestScope(ctx, conn, site, req.Room)
				if !ok {
					continue
				}
				if !s.realtimeLimit.Allow(s.clientIP(r)) {
					writeWSError(ctx, conn, "rate limit exceeded, slow down")
					continue
				}
				if roomIdentity == nil {
					id, ok := s.websocketIdentity(ctx, conn, r)
					if !ok {
						continue
					}
					roomIdentity = &id
				}
				s.roomHub.Join(scope, req.Room, roomUserFromIdentity(sessionID, *roomIdentity), roomOut)
			case "room_leave":
				scope, ok := s.roomRequestScope(ctx, conn, site, req.Room)
				if !ok {
					continue
				}
				s.roomHub.Leave(scope, req.Room, sessionID)
			case "room_presence":
				scope, ok := s.roomRequestScope(ctx, conn, site, req.Room)
				if !ok {
					continue
				}
				if !validRealtimePayload(ctx, conn, req.Data) {
					continue
				}
				if !s.realtimeLimit.Allow(s.clientIP(r)) {
					writeWSError(ctx, conn, "rate limit exceeded, slow down")
					continue
				}
				if !s.roomHub.SetPresence(scope, req.Room, sessionID, req.Data) {
					writeWSError(ctx, conn, "join room before setting presence")
				}
			case "room_send":
				scope, ok := s.roomRequestScope(ctx, conn, site, req.Room)
				if !ok {
					continue
				}
				if !roomEventRe.MatchString(req.Event) {
					writeWSError(ctx, conn, "invalid room event name")
					continue
				}
				if !validRealtimePayload(ctx, conn, req.Data) {
					continue
				}
				if !s.realtimeLimit.Allow(s.clientIP(r)) {
					writeWSError(ctx, conn, "rate limit exceeded, slow down")
					continue
				}
				if !s.roomHub.Publish(scope, req.Room, sessionID, req.Event, req.Data) {
					writeWSError(ctx, conn, "join room before sending messages")
				}
			default:
				writeWSError(ctx, conn, "unknown request type "+req.Type)
			}
		case ev := <-docOut:
			if err := wsjson.Write(ctx, conn, ev); err != nil {
				return
			}
		case ev := <-roomOut:
			if err := wsjson.Write(ctx, conn, ev); err != nil {
				return
			}
		}
	}
}

func (s *Server) roomRequestScope(ctx context.Context, conn *websocket.Conn, site, room string) (string, bool) {
	scope, err := roomScopeFor(site, room)
	if err != nil {
		writeWSError(ctx, conn, err.Error())
		return "", false
	}
	return scope, true
}

func (s *Server) websocketIdentity(ctx context.Context, conn *websocket.Conn, r *http.Request) (Identity, bool) {
	if id, ok := s.forwardAuthIdentity(r); ok {
		return id, true
	}
	if s.resolver == nil {
		writeWSError(ctx, conn,
			"identity resolver not configured: set SPOT_AUTH_MODE=single-user, NETBIRD_API_URL/NETBIRD_API_TOKEN, TAILSCALE_API_TOKEN, TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET, SPOT_FORWARD_AUTH, or explicit dev identity")
		return Identity{}, false
	}
	ip := s.clientIP(r)
	id, found, err := s.resolver.Resolve(ctx, ip)
	if err != nil {
		log.Printf("realtime: resolve %s: %v", ip, err)
		writeWSError(ctx, conn, "could not reach the identity resolver")
		return Identity{}, false
	}
	if !found {
		writeWSError(ctx, conn, "no identity matches "+ip)
		return Identity{}, false
	}
	if id.PeerIP == "" {
		id.PeerIP = ip
	}
	return id, true
}

func validRealtimePayload(ctx context.Context, conn *websocket.Conn, data json.RawMessage) bool {
	if len(data) > maxRealtimeData {
		writeWSError(ctx, conn, fmt.Sprintf("room data exceeds the %d KB limit", maxRealtimeData>>10))
		return false
	}
	return true
}

func writeWSError(ctx context.Context, conn *websocket.Conn, msg string) {
	_ = wsjson.Write(ctx, conn, map[string]string{"type": "error", "error": msg})
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
	owner, ok := s.mineOwner(w, r)
	if !ok {
		return
	}
	// A batch fetch by id short-circuits filters, sort, and paging.
	if raw := r.URL.Query().Get("ids"); raw != "" {
		ids := strings.Split(raw, ",")
		if len(ids) > maxBatchIDs {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("ids accepts at most %d document UUIDs", maxBatchIDs))
			return
		}
		for _, id := range ids {
			if !idRe.MatchString(id) {
				httpError(w, http.StatusBadRequest, "ids must be a comma-separated list of document UUIDs")
				return
			}
		}
		var docs []Document
		var err error
		if owner != "" {
			docs, err = s.store.GetManyOwned(r.Context(), scope, collection, ids, owner)
		} else {
			docs, err = s.store.GetMany(r.Context(), scope, collection, ids)
		}
		if err != nil {
			s.storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
		return
	}

	after := r.URL.Query().Get("after")
	if after != "" && !idRe.MatchString(after) {
		httpError(w, http.StatusBadRequest, "after must be a document UUID")
		return
	}
	where, ok := parseWhere(w, r)
	if !ok {
		return
	}
	sort := r.URL.Query().Get("sort")
	if after != "" && sort != "" {
		httpError(w, http.StatusBadRequest, "after cursor is only supported for the default order; drop sort or after")
		return
	}
	docs, err := s.store.Query(r.Context(), scope, collection, ListQuery{
		Limit: limit,
		Owner: owner,
		After: after,
		Where: where,
		Sort:  sort,
		Order: r.URL.Query().Get("order"),
	})
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// maxBatchIDs caps how many ids a single getMany request may fetch.
const maxBatchIDs = 100

func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	owner, ok := s.mineOwner(w, r)
	if !ok {
		return
	}
	where, ok := parseWhere(w, r)
	if !ok {
		return
	}
	n, err := s.store.Count(r.Context(), scope, collection, owner, where)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": n})
}

func (s *Server) handleIncrement(w http.ResponseWriter, r *http.Request) {
	scope, collection, ok := s.scope(w, r)
	if !ok {
		return
	}
	id, ok := readID(w, r)
	if !ok {
		return
	}
	var body struct {
		Field string   `json:"field"`
		By    *float64 `json:"by"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	if err := dec.Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, `request body must be {"field": string, "by"?: number}`)
		return
	}
	if body.Field == "" {
		httpError(w, http.StatusBadRequest, "field is required")
		return
	}
	by := 1.0
	if body.By != nil {
		by = *body.By
	}
	owner, ok := s.mineOwner(w, r)
	if !ok {
		return
	}
	// The store treats an empty owner as "no ownership filter", so the owned
	// call covers both the mine and non-mine cases.
	doc, err := s.store.IncrementOwned(r.Context(), scope, collection, id, owner, body.Field, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// parseWhere reads an optional `where` query parameter — a JSON object of
// field -> value (equality) or field -> {op: value} — into filters. Field
// names and operators are validated by the document store.
func parseWhere(w http.ResponseWriter, r *http.Request) ([]Filter, bool) {
	raw := r.URL.Query().Get("where")
	if raw == "" {
		return nil, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		httpError(w, http.StatusBadRequest, "where must be a JSON object")
		return nil, false
	}
	if obj == nil {
		httpError(w, http.StatusBadRequest, "where must be a JSON object")
		return nil, false
	}
	filters := make([]Filter, 0, len(obj))
	for field, rawVal := range obj {
		// A nested object is an operator map ({gte: 3}); anything else is an
		// equality test against the value.
		if trimmed := strings.TrimSpace(string(rawVal)); strings.HasPrefix(trimmed, "{") {
			var ops map[string]json.RawMessage
			if err := json.Unmarshal(rawVal, &ops); err != nil {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("where filter %q is malformed", field))
				return nil, false
			}
			// An empty operator object would otherwise contribute no filter and
			// silently widen the query to the whole collection; reject it.
			if len(ops) == 0 {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("where filter %q must name at least one operator", field))
				return nil, false
			}
			for op, rawOpVal := range ops {
				f := Filter{Field: field, Op: op}
				if op == "in" {
					var arr []any
					if err := json.Unmarshal(rawOpVal, &arr); err != nil {
						httpError(w, http.StatusBadRequest, fmt.Sprintf("where filter %q: in needs an array", field))
						return nil, false
					}
					f.Value = arr
				} else {
					var v any
					if err := json.Unmarshal(rawOpVal, &v); err != nil {
						httpError(w, http.StatusBadRequest, fmt.Sprintf("where filter %q is malformed", field))
						return nil, false
					}
					f.Value = v
				}
				filters = append(filters, f)
			}
			continue
		}
		var v any
		if err := json.Unmarshal(rawVal, &v); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("where filter %q is malformed", field))
			return nil, false
		}
		filters = append(filters, Filter{Field: field, Op: "eq", Value: v})
	}
	return filters, true
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
	// Ownership is best-effort on create: a resolver outage degrades to an
	// unattributed (empty-owner) document rather than failing the write, so an
	// open site keeps accepting writes during a transient mesh blip. Restricted
	// sites already required a resolved identity in s.scope's authorizeSiteAccess.
	owner, err := s.callerKey(r)
	if err != nil {
		log.Printf("create: resolve owner (continuing unattributed): %v", err)
		owner = ""
	}
	doc, err := s.store.Create(r.Context(), scope, collection, owner, data)
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
	owner, ok := s.mineOwner(w, r)
	if !ok {
		return
	}
	// The store treats an empty owner as "no ownership filter", so the owned
	// call covers both the mine and non-mine cases.
	doc, err := s.store.UpdateOwned(r.Context(), scope, collection, id, owner, data)
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
	owner, ok := s.mineOwner(w, r)
	if !ok {
		return
	}
	// The store treats an empty owner as "no ownership filter", so the owned
	// call covers both the mine and non-mine cases.
	if err := s.store.DeleteOwned(r.Context(), scope, collection, id, owner); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mineOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	mine := r.URL.Query().Get("mine")
	if mine != "1" && mine != "true" {
		return "", true
	}
	owner, err := s.callerKey(r)
	if err != nil {
		log.Printf("mine: resolve owner: %v", err)
		httpError(w, http.StatusServiceUnavailable, "could not reach the identity resolver")
		return "", false
	}
	if owner == "" {
		httpError(w, http.StatusBadRequest, "mine=true requires an identified visitor")
		return "", false
	}
	return owner, true
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpError(w, http.StatusNotFound, "document not found")
	case errors.Is(err, ErrBadQuery):
		// ErrBadQuery messages are built from validated tokens only.
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrFieldNotNumeric):
		httpError(w, http.StatusConflict, "cannot increment a non-numeric field")
	default:
		log.Printf("docstore: %v", err)
		httpError(w, http.StatusInternalServerError, "database error")
	}
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
