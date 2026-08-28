package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	publishingKeyTokenPrefix = "spot_pk_"
	publishingKeyIDBytes     = 16
	publishingKeySecretBytes = 32
	maxPublishingKeyBody     = 16 << 10
)

var (
	errPublishingKeyInvalid  = errors.New("invalid publishing key")
	errPublishingKeyNotFound = errors.New("publishing key not found")
	publishingKeyPrefixRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}-$`)
)

type PublishingKey struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	SitePrefix  string     `json:"site_prefix"`
	OwnerEmail  string     `json:"owner_email,omitempty"`
	OwnerPeerIP string     `json:"owner_peer_ip,omitempty"`
	OwnerName   string     `json:"owner_name,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type DeployPrincipal struct {
	Actor          Identity
	AuthMethod     string
	PublisherKeyID string
	PublisherName  string
	RequiredPrefix string
	PublishingKey  bool
}

type deployPrincipalContextKey struct{}

func withDeployPrincipal(r *http.Request, principal DeployPrincipal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), deployPrincipalContextKey{}, principal))
}

func deployPrincipalFromRequest(r *http.Request) (DeployPrincipal, bool) {
	principal, ok := r.Context().Value(deployPrincipalContextKey{}).(DeployPrincipal)
	return principal, ok
}

func (k PublishingKey) ownerIdentity() Identity {
	return Identity{Email: k.OwnerEmail, PeerIP: k.OwnerPeerIP, Name: k.OwnerName, Groups: []string{}}
}

type PublishingKeyStore struct {
	db   *sql.DB
	rand io.Reader
}

func NewPublishingKeyStore(db *sql.DB) *PublishingKeyStore {
	return &PublishingKeyStore{db: db, rand: rand.Reader}
}

func validatePublishingKeyName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("name is required")
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return "", errors.New("name must be at most 80 characters on one line")
	}
	return name, nil
}

func validatePublishingKeyPrefix(prefix string) error {
	if !publishingKeyPrefixRe.MatchString(prefix) {
		return errors.New("site_prefix must be 2-61 lowercase letters, digits, or hyphens, starting with a letter or digit and ending with a hyphen")
	}
	return nil
}

func (s *PublishingKeyStore) Create(ctx context.Context, actor Identity, rawName, sitePrefix string) (PublishingKey, string, error) {
	if actorKey(actor) == "" {
		return PublishingKey{}, "", ErrDeployForbidden
	}
	name, err := validatePublishingKeyName(rawName)
	if err != nil {
		return PublishingKey{}, "", err
	}
	if err := validatePublishingKeyPrefix(sitePrefix); err != nil {
		return PublishingKey{}, "", err
	}
	idRaw := make([]byte, publishingKeyIDBytes)
	secretRaw := make([]byte, publishingKeySecretBytes)
	if _, err := io.ReadFull(s.rand, idRaw); err != nil {
		return PublishingKey{}, "", fmt.Errorf("generate publishing key id: %w", err)
	}
	if _, err := io.ReadFull(s.rand, secretRaw); err != nil {
		return PublishingKey{}, "", fmt.Errorf("generate publishing key secret: %w", err)
	}
	id := hex.EncodeToString(idRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	hash := sha256.Sum256(secretRaw)
	key := PublishingKey{
		ID: id, Name: name, SitePrefix: sitePrefix,
		OwnerEmail: strings.ToLower(actor.Email), OwnerPeerIP: actor.PeerIP,
		OwnerName: ownerNameForIdentity(actor),
	}
	if key.OwnerEmail != "" {
		key.OwnerPeerIP = ""
	}
	err = s.db.QueryRowContext(ctx, `INSERT INTO publishing_keys
		(id, owner_email, owner_peer_ip, owner_name, name, site_prefix, secret_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING created_at`, key.ID, key.OwnerEmail, key.OwnerPeerIP, key.OwnerName,
		key.Name, key.SitePrefix, hash[:]).Scan(&key.CreatedAt)
	if err != nil {
		return PublishingKey{}, "", fmt.Errorf("create publishing key: %w", err)
	}
	return key, publishingKeyTokenPrefix + id + "_" + secret, nil
}

func (s *PublishingKeyStore) List(ctx context.Context, actor Identity) ([]PublishingKey, error) {
	var rows *sql.Rows
	var err error
	if actor.Email != "" && actor.PeerIP != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, owner_email, owner_peer_ip, owner_name,
			name, site_prefix, created_at, last_used_at, revoked_at
			FROM publishing_keys
			WHERE owner_email = ? OR (owner_email = '' AND owner_peer_ip = ?)
			ORDER BY created_at DESC, id DESC`, strings.ToLower(actor.Email), actor.PeerIP)
	} else if actor.Email != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, owner_email, owner_peer_ip, owner_name,
			name, site_prefix, created_at, last_used_at, revoked_at
			FROM publishing_keys WHERE owner_email = ?
			ORDER BY created_at DESC, id DESC`, strings.ToLower(actor.Email))
	} else if actor.PeerIP != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT id, owner_email, owner_peer_ip, owner_name,
			name, site_prefix, created_at, last_used_at, revoked_at
			FROM publishing_keys WHERE owner_email = '' AND owner_peer_ip = ?
			ORDER BY created_at DESC, id DESC`, actor.PeerIP)
	} else {
		return nil, ErrDeployForbidden
	}
	if err != nil {
		return nil, fmt.Errorf("list publishing keys: %w", err)
	}
	defer rows.Close()
	keys := []PublishingKey{}
	for rows.Next() {
		key, err := scanPublishingKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list publishing keys: %w", err)
	}
	return keys, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanPublishingKey(row rowScanner) (PublishingKey, error) {
	var key PublishingKey
	var lastUsed, revoked sql.NullTime
	if err := row.Scan(&key.ID, &key.OwnerEmail, &key.OwnerPeerIP, &key.OwnerName,
		&key.Name, &key.SitePrefix, &key.CreatedAt, &lastUsed, &revoked); err != nil {
		return PublishingKey{}, fmt.Errorf("scan publishing key: %w", err)
	}
	if lastUsed.Valid {
		key.LastUsedAt = &lastUsed.Time
	}
	if revoked.Valid {
		key.RevokedAt = &revoked.Time
	}
	return key, nil
}

func parsePublishingKeyToken(token string) (string, []byte, error) {
	if !strings.HasPrefix(token, publishingKeyTokenPrefix) {
		return "", nil, errPublishingKeyInvalid
	}
	rest := strings.TrimPrefix(token, publishingKeyTokenPrefix)
	idLen := publishingKeyIDBytes * 2
	if len(rest) <= idLen || rest[idLen] != '_' {
		return "", nil, errPublishingKeyInvalid
	}
	id := rest[:idLen]
	if _, err := hex.DecodeString(id); err != nil || id != strings.ToLower(id) {
		return "", nil, errPublishingKeyInvalid
	}
	encodedSecret := rest[idLen+1:]
	secret, err := base64.RawURLEncoding.DecodeString(encodedSecret)
	if err != nil || len(secret) != publishingKeySecretBytes ||
		base64.RawURLEncoding.EncodeToString(secret) != encodedSecret {
		return "", nil, errPublishingKeyInvalid
	}
	return id, secret, nil
}

func publishingKeyPublicID(token string) string {
	if !strings.HasPrefix(token, publishingKeyTokenPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(token, publishingKeyTokenPrefix)
	idLen := publishingKeyIDBytes * 2
	if len(rest) <= idLen || rest[idLen] != '_' {
		return ""
	}
	id := rest[:idLen]
	if _, err := hex.DecodeString(id); err != nil || id != strings.ToLower(id) {
		return ""
	}
	return id
}

func logPublishingKeyRejection(token, site string) {
	id := publishingKeyPublicID(token)
	switch {
	case id != "" && site != "":
		log.Printf("publishing key rejected: id=%s site=%s", id, site)
	case id != "":
		log.Printf("publishing key authentication rejected: id=%s", id)
	default:
		log.Printf("publishing key authentication rejected")
	}
}

func (s *PublishingKeyStore) Authenticate(ctx context.Context, token string) (PublishingKey, error) {
	id, secret, err := parsePublishingKeyToken(token)
	if err != nil {
		return PublishingKey{}, errPublishingKeyInvalid
	}
	var key PublishingKey
	var storedHash []byte
	var lastUsed, revoked sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT id, owner_email, owner_peer_ip, owner_name,
		name, site_prefix, created_at, last_used_at, revoked_at, secret_hash
		FROM publishing_keys WHERE id = ?`, id).Scan(
		&key.ID, &key.OwnerEmail, &key.OwnerPeerIP, &key.OwnerName,
		&key.Name, &key.SitePrefix, &key.CreatedAt, &lastUsed, &revoked, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishingKey{}, errPublishingKeyInvalid
	}
	if err != nil {
		return PublishingKey{}, fmt.Errorf("authenticate publishing key: %w", err)
	}
	if revoked.Valid || len(storedHash) != sha256.Size {
		return PublishingKey{}, errPublishingKeyInvalid
	}
	hash := sha256.Sum256(secret)
	if subtle.ConstantTimeCompare(storedHash, hash[:]) != 1 {
		return PublishingKey{}, errPublishingKeyInvalid
	}
	if lastUsed.Valid {
		key.LastUsedAt = &lastUsed.Time
	}
	return key, nil
}

func (s *PublishingKeyStore) TouchUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE publishing_keys
		SET last_used_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE id = ? AND revoked_at IS NULL AND secret_hash IS NOT NULL`, id)
	return err
}

func (s *PublishingKeyStore) Revoke(ctx context.Context, id string, actor Identity, admin bool) error {
	if len(id) != publishingKeyIDBytes*2 {
		return errPublishingKeyNotFound
	}
	var ownerEmail, ownerPeerIP string
	err := s.db.QueryRowContext(ctx, `SELECT owner_email, owner_peer_ip FROM publishing_keys WHERE id = ?`, id).Scan(&ownerEmail, &ownerPeerIP)
	if errors.Is(err, sql.ErrNoRows) {
		return errPublishingKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("read publishing key owner: %w", err)
	}
	owned := ownerEmail != "" && actor.Email != "" && strings.EqualFold(ownerEmail, actor.Email)
	if ownerEmail == "" {
		owned = ownerPeerIP != "" && actor.PeerIP == ownerPeerIP
	}
	if !owned && !admin {
		return errPublishingKeyNotFound
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE publishing_keys
		SET revoked_at = COALESCE(revoked_at, strftime('%Y-%m-%d %H:%M:%f', 'now')),
			secret_hash = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("revoke publishing key: %w", err)
	}
	return nil
}

func (s *Server) requireDeployPrincipal(w http.ResponseWriter, r *http.Request) (DeployPrincipal, bool) {
	values := r.Header.Values("Authorization")
	if len(values) > 0 {
		if len(values) != 1 {
			logPublishingKeyRejection("", "")
			httpError(w, http.StatusUnauthorized, "invalid or revoked publishing key")
			return DeployPrincipal{}, false
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || s.publishingKeys == nil {
			logPublishingKeyRejection("", "")
			httpError(w, http.StatusUnauthorized, "invalid or revoked publishing key")
			return DeployPrincipal{}, false
		}
		key, err := s.publishingKeys.Authenticate(r.Context(), parts[1])
		if err != nil {
			if !errors.Is(err, errPublishingKeyInvalid) {
				log.Printf("publishing key authentication: %v", err)
				httpError(w, http.StatusServiceUnavailable, "could not authenticate publishing key")
				return DeployPrincipal{}, false
			}
			logPublishingKeyRejection(parts[1], "")
			httpError(w, http.StatusUnauthorized, "invalid or revoked publishing key")
			return DeployPrincipal{}, false
		}
		return DeployPrincipal{
			Actor: key.ownerIdentity(), AuthMethod: "publishing_key",
			PublisherKeyID: key.ID, PublisherName: key.Name,
			RequiredPrefix: key.SitePrefix, PublishingKey: true,
		}, true
	}
	id, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return DeployPrincipal{}, false
	}
	method := s.deployAuthMethod
	if method == "" {
		method = "identity"
	}
	if _, ok := s.forwardAuthIdentity(r); ok {
		method = "forward_auth"
	} else if s.deployAuthMethod == "" {
		switch s.resolver.(type) {
		case *NetbirdResolver:
			method = "netbird"
		case *TailscaleResolver:
			method = "tailscale"
		case *StaticResolver:
			method = "static"
		}
	}
	return DeployPrincipal{Actor: id, AuthMethod: method}, true
}

type publishingKeyCreateRequest struct {
	Name       string `json:"name"`
	SitePrefix string `json:"site_prefix"`
}

func (s *Server) handlePublishingKeys(w http.ResponseWriter, r *http.Request) {
	if siteFromHost(s.requestHost(r), s.spotDomain) != "" {
		httpError(w, http.StatusBadRequest, "publishing keys are managed on the platform root")
		return
	}
	if s.publishingKeys == nil {
		httpError(w, http.StatusServiceUnavailable, "publishing keys are not configured")
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := s.publishingKeys.List(r.Context(), actor)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "could not list publishing keys")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxPublishingKeyBody)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var request publishingKeyCreateRequest
		if err := dec.Decode(&request); err != nil {
			httpError(w, http.StatusBadRequest, "invalid publishing key request")
			return
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			httpError(w, http.StatusBadRequest, "publishing key request must contain one JSON object")
			return
		}
		key, secret, err := s.publishingKeys.Create(r.Context(), actor, request.Name, request.SitePrefix)
		if err != nil {
			if errors.Is(err, ErrDeployForbidden) {
				httpError(w, http.StatusForbidden, "publishing key creation requires a stable identity")
				return
			}
			if strings.Contains(err.Error(), "name") || strings.Contains(err.Error(), "site_prefix") {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			httpError(w, http.StatusInternalServerError, "could not create publishing key")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
	default:
		w.Header().Set("Allow", "GET, POST")
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handlePublishingKey(w http.ResponseWriter, r *http.Request) {
	if siteFromHost(s.requestHost(r), s.spotDomain) != "" {
		httpError(w, http.StatusBadRequest, "publishing keys are managed on the platform root")
		return
	}
	if s.publishingKeys == nil {
		httpError(w, http.StatusServiceUnavailable, "publishing keys are not configured")
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	admin := allowsAdmin(s.adminPolicy, actor)
	if err := s.publishingKeys.Revoke(r.Context(), id, actor, admin); err != nil {
		if errors.Is(err, errPublishingKeyNotFound) {
			httpError(w, http.StatusNotFound, "publishing key not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "could not revoke publishing key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
