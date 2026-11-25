package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

// The site's owner sees a bar at the top of HTML pages saying who can open them,
// rendered per request from the policy that authorized the request, so it can
// never disagree with the policy. It names no one: the allow list is visible
// only to the site's managers. Pages published to Cloudflare are read from
// storage, not served here, so internet copies carry no bar.

const (
	accessBarHeight = "28px"
	// Part of every bar validator: a change to the markup or the insertion
	// rule must not be answered with a 304 for a page rendered the old way.
	accessBarTransformVersion = "2"
)

type accessBarLevel struct {
	label, color, text string
}

// openAudience names who can open an unrestricted site. Spot knows it is on a
// mesh only when it resolves visitors through the mesh API alone; a forward
// auth proxy or login app may admit visitors from elsewhere, and an open site
// needs no identity, so reaching Spot is then the only limit.
func (s *Server) openAudience() string {
	if s.login == nil && s.forwardAuth == nil {
		switch s.deployAuthMethod {
		case "netbird":
			return "Everyone on the mesh"
		case "tailscale":
			return "Everyone on the tailnet"
		}
	}
	return "Anyone who can reach this Spot"
}

// fixedIdentity is the identity every visitor gets in single-user and dev
// mode, where a restriction that admits it admits anyone who can reach Spot.
func (s *Server) fixedIdentity() (Identity, bool) {
	if s.deployAuthMethod != "single_user" && s.deployAuthMethod != "dev" {
		return Identity{}, false
	}
	static, ok := s.resolver.(*StaticResolver)
	if !ok {
		return Identity{}, false
	}
	return static.identity, true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// accessBarLevelFor counts distinct allow entries the way authorization
// matches them: trimmed, case-insensitive, emails apart from groups.
func accessBarLevelFor(policy *AccessPolicy, openAudience, target string, fixed *Identity) accessBarLevel {
	open := accessBarLevel{"OPEN", "#15803d", openAudience + " can open " + target}
	if !policy.RestrictsAccess() {
		return open
	}
	if fixed != nil && policy.Allows(*fixed) {
		open.text = "Anyone who can reach this Spot can open " + target
		return open
	}
	emails, groups := map[string]bool{}, map[string]bool{}
	for _, entry := range policy.Allow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		switch {
		case entry == "":
		case strings.Contains(entry, "@"):
			emails[entry] = true
		default:
			groups[entry] = true
		}
	}
	switch {
	case len(emails) == 0 && len(groups) == 0:
		return accessBarLevel{"LOCKED", "#b91c1c", "Nobody can open " + target}
	case len(emails) == 1 && len(groups) == 0:
		return accessBarLevel{"PRIVATE", "#b91c1c", "Only you can open " + target}
	}
	var parts []string
	if len(emails) > 0 {
		parts = append(parts, plural(len(emails), "email address", "email addresses"))
	}
	if len(groups) > 0 {
		parts = append(parts, plural(len(groups), "group", "groups"))
	}
	text := "Limited to " + strings.Join(parts, " and ")
	if target != "this page" {
		text += " on " + target
	}
	return accessBarLevel{"SHARED", "#6d28d9", text}
}

// accessBarMarkupFor renders the bar for the policy that authorized the
// request. A site with a Cloudflare publication row has a separately
// configured internet copy, so the bar scopes its claim to this Spot copy.
func (s *Server) accessBarMarkupFor(r *http.Request, site string, policy *AccessPolicy) (string, error) {
	if !s.showAccessBar(r, site) {
		return "", nil
	}
	ctx := r.Context()
	target := "this page"
	if s.cloudflarePubs != nil {
		published, err := s.cloudflarePubs.Has(ctx, site)
		if err != nil {
			return "", fmt.Errorf("read cloudflare publication: %w", err)
		}
		if published {
			target = "this Spot copy"
		}
	}
	var fixed *Identity
	if id, ok := s.fixedIdentity(); ok {
		fixed = &id
	}
	return accessBarMarkup(accessBarLevelFor(policy, s.openAudience(), target, fixed)), nil
}

// showAccessBar recognizes the immutable owner, rather than broader management
// access. An unavailable identity or registry leaves the page unadorned.
func (s *Server) showAccessBar(r *http.Request, site string) bool {
	manager, ok := s.siteManager.(siteAccessManager)
	if !ok {
		return false
	}
	actor, found, err := s.resolvePeer(r)
	if err != nil {
		log.Printf("access bar %s: resolve owner identity: %v", site, err)
		return false
	}
	if !found {
		return false
	}
	decision, err := manager.ManagementDecision(r.Context(), site, actor)
	if err != nil {
		log.Printf("access bar %s: verify owner: %v", site, err)
		return false
	}
	return decision.Allowed && decision.Role == ManagementRoleOwner
}

// accessBarMarkup puts the bar's own styles in a declarative shadow root, so
// site selectors do not reach them. The bar is fixed across the viewport and
// the page moves down by --spot-access-bar-height, which pages with their own
// sticky or fixed headers can offset by.
func accessBarMarkup(level accessBarLevel) string {
	text := html.EscapeString(level.text)
	return `<spot-access-bar role="note" aria-label="Who can open this page" style="all:initial;display:block;position:fixed;top:0;left:0;right:0;z-index:2147483647">` +
		`<template shadowrootmode="open"><style>` +
		`div{box-sizing:border-box;height:` + accessBarHeight + `;display:flex;align-items:center;justify-content:center;gap:10px;padding:0 16px;` +
		`background:` + level.color + `;color:#fff;font:600 13px/1 system-ui,-apple-system,"Segoe UI",sans-serif}` +
		`b{flex:none;font-weight:800;letter-spacing:.12em}` +
		`span{min-width:0;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}` +
		`@media (max-width:480px){div{gap:8px;padding:0 10px;font-size:12px}}` +
		`</style><div><b>` + level.label + `</b><span title="` + text + `">` + text + `</span></div></template></spot-access-bar>` +
		`<style>:root{--spot-access-bar-height:` + accessBarHeight + `}` +
		`html{padding-top:var(--spot-access-bar-height)!important;scroll-padding-top:var(--spot-access-bar-height)}</style>`
}

// accessBarETag derives a strong validator from the stored file's validator
// and the exact bar, without reading the page.
func accessBarETag(prefix, sourceTag, markup string) string {
	sum := sha256.Sum256([]byte(accessBarTransformVersion + "\x00" + sourceTag + "\x00" + markup))
	return `"` + prefix + hex.EncodeToString(sum[:]) + `"`
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Elements the HTML parser keeps in <head>; anything else, or non-whitespace
// text outside them, opens the body.
var accessBarHeadTags = map[string]bool{
	"html": true, "head": true, "base": true, "basefont": true, "bgsound": true, "link": true,
	"meta": true, "title": true, "noscript": true, "noframes": true, "style": true, "script": true,
	"template": true,
}

// accessBarInsertOffset returns the byte offset where the body's content
// starts: right after <body ...>, or where the parser would open the body
// implicitly. Offsets come from the tokenizer's raw token lengths, so they
// index the original bytes. A leading BOM stays first.
func accessBarInsertOffset(r io.Reader) (int64, error) {
	br := bufio.NewReader(r)
	var offset int64
	if head, err := br.Peek(len(utf8BOM)); err == nil && bytes.Equal(head, utf8BOM) {
		_, _ = br.Discard(len(utf8BOM))
		offset = int64(len(utf8BOM))
	}
	z := xhtml.NewTokenizer(br)
	templateDepth := 0
	rawText := false
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			if errors.Is(z.Err(), io.EOF) {
				return offset, nil
			}
			return 0, z.Err()
		}
		size := int64(len(z.Raw()))
		// The tokenizer hands the content of script, style, title and the
		// like to us as the text token right after their start tag.
		inRawText := rawText
		rawText = false
		var name string
		if tt == xhtml.StartTagToken || tt == xhtml.SelfClosingTagToken || tt == xhtml.EndTagToken {
			raw, _ := z.TagName()
			name = string(raw)
		}
		switch {
		case name == "template" && tt == xhtml.StartTagToken:
			templateDepth++
		case name == "template" && tt == xhtml.EndTagToken && templateDepth > 0:
			templateDepth--
		case templateDepth > 0:
			// Template contents never open the body.
		case tt == xhtml.StartTagToken || tt == xhtml.SelfClosingTagToken:
			if name == "body" {
				return offset + size, nil
			}
			if !accessBarHeadTags[name] {
				return offset, nil
			}
			rawText = tt == xhtml.StartTagToken && accessBarRawTextTags[name]
		case tt == xhtml.EndTagToken:
			if !accessBarHeadTags[name] {
				return offset, nil
			}
		case tt == xhtml.TextToken:
			// Classify the decoded text ("&#32;" is whitespace); offsets
			// still use the raw length taken above.
			if !inRawText && strings.Trim(string(z.Text()), " \t\n\f\r") != "" {
				return offset, nil
			}
		}
		offset += size
	}
}

// Head elements whose content the tokenizer reads as raw text.
var accessBarRawTextTags = map[string]bool{"title": true, "style": true, "script": true, "noscript": true, "noframes": true}

// splicedPage serves source[:at] + insert + source[at:] as one seekable body,
// so ServeContent keeps HEAD, Range and preconditions without buffering.
type splicedPage struct {
	src      io.ReadSeeker
	srcPos   int64
	at, size int64
	insert   []byte
	pos      int64
}

func (p *splicedPage) total() int64 { return p.size + int64(len(p.insert)) }

func (p *splicedPage) Read(b []byte) (int, error) {
	if p.pos >= p.total() {
		return 0, io.EOF
	}
	insertEnd := p.at + int64(len(p.insert))
	if p.pos >= p.at && p.pos < insertEnd {
		n := copy(b, p.insert[p.pos-p.at:])
		p.pos += int64(n)
		return n, nil
	}
	srcOff, limit := p.pos, p.at-p.pos
	if p.pos >= insertEnd {
		srcOff, limit = p.pos-int64(len(p.insert)), p.total()-p.pos
	}
	if int64(len(b)) > limit {
		b = b[:limit]
	}
	if p.srcPos != srcOff {
		if _, err := p.src.Seek(srcOff, io.SeekStart); err != nil {
			return 0, err
		}
		p.srcPos = srcOff
	}
	n, err := p.src.Read(b)
	p.srcPos += int64(n)
	p.pos += int64(n)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	return n, err
}

func (p *splicedPage) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += p.pos
	case io.SeekEnd:
		offset += p.total()
	default:
		return 0, errors.New("splicedPage: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("splicedPage: negative position")
	}
	p.pos = offset
	return offset, nil
}

// effectiveContentType is the type ServeContent would send: by extension,
// else sniffed from the first 512 bytes. The returned reader yields the whole
// file: a seekable source is rewound, any other gets the sniffed bytes back.
func effectiveContentType(name string, src io.Reader) (string, io.Reader, error) {
	if ctype := mime.TypeByExtension(path.Ext(name)); ctype != "" {
		return ctype, src, nil
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", nil, err
	}
	ctype := http.DetectContentType(head[:n])
	if seeker, ok := src.(io.ReadSeeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return "", nil, err
		}
		return ctype, src, nil
	}
	return ctype, io.MultiReader(bytes.NewReader(head[:n]), src), nil
}

// notModifiedShortcut answers 304 before the body is read. Anything else,
// If-Match included, is left to ServeContent's full precondition order.
func notModifiedShortcut(r *http.Request, etag string) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("If-Match") == "" && ifNoneMatchMatches(r.Header.Get("If-None-Match"), etag)
}

func isHTMLType(ctype string) bool {
	mediaType, _, _ := mime.ParseMediaType(ctype)
	return mediaType == "text/html"
}

// writeHTMLWithAccessBar serves an HTML page with markup at the start of its
// body. The caller has set the validator and cache headers. Both shipped
// stores are seekable; another source is read into memory. A UTF-16 page,
// which the UTF-8 markup would corrupt, is served as stored.
func writeHTMLWithAccessBar(w http.ResponseWriter, r *http.Request, name, ctype, markup string, src io.Reader) {
	fail := func(err error) {
		log.Printf("access bar %s: %v", name, err)
		httpError(w, http.StatusInternalServerError, "could not read site file")
	}
	seeker, ok := src.(io.ReadSeeker)
	if !ok {
		page, err := io.ReadAll(src)
		if err != nil {
			fail(err)
			return
		}
		seeker = bytes.NewReader(page)
	}
	size, err := seeker.Seek(0, io.SeekEnd)
	var at int64
	if err == nil {
		_, err = seeker.Seek(0, io.SeekStart)
	}
	var bom [2]byte
	if err == nil {
		_, err = io.ReadFull(seeker, bom[:])
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			err = nil
		}
	}
	utf16 := bom == [2]byte{0xFE, 0xFF} || bom == [2]byte{0xFF, 0xFE}
	if err == nil && !utf16 {
		if _, err = seeker.Seek(0, io.SeekStart); err == nil {
			// Memory grows with the longest token before <body>, bounded by
			// the deploy size limit.
			at, err = accessBarInsertOffset(seeker)
		}
	}
	if err == nil {
		_, err = seeker.Seek(0, io.SeekStart)
	}
	if err != nil {
		fail(err)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Del("Last-Modified")
	if utf16 {
		http.ServeContent(w, r, name, time.Time{}, seeker)
		return
	}
	page := &splicedPage{src: seeker, srcPos: -1, at: at, size: size, insert: []byte(markup)}
	http.ServeContent(w, r, name, time.Time{}, page)
}
