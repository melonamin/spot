package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Every HTML page of a site shows a sticky bar that says who can open it,
// rendered from the site's current access policy at request time, so it can
// never disagree with the policy. Pages published to Cloudflare are read
// from storage, not served here, so internet copies carry no bar.

const (
	accessBarHeight = "28px"
	// Larger pages are served as deployed rather than buffered in memory.
	maxAccessBarPageSize = 32 << 20
)

type accessBarLevel struct {
	label, color, text string
}

func accessBarFor(policy *AccessPolicy) accessBarLevel {
	if !policy.RestrictsAccess() {
		return accessBarLevel{"OPEN", "#15803d", "Everyone on the mesh can open this page"}
	}
	switch {
	case len(policy.Allow) == 0:
		return accessBarLevel{"LOCKED", "#b91c1c", "Nobody can open this page"}
	case len(policy.Allow) == 1 && strings.Contains(policy.Allow[0], "@"):
		return accessBarLevel{"PRIVATE", "#b91c1c", "Only " + policy.Allow[0] + " can open this page"}
	default:
		return accessBarLevel{"SHARED", "#6d28d9", strings.Join(policy.Allow, ", ") + " can open this page"}
	}
}

// accessBarMarkup isolates the bar in a declarative shadow root, so site CSS
// cannot restyle or hide it. The bar is fixed across the full viewport width,
// whatever the body's own margins, and the page moves down by its height,
// published as --spot-access-bar-height for pages with their own sticky headers.
func accessBarMarkup(level accessBarLevel) string {
	text := html.EscapeString(level.text)
	return `<spot-access-bar role="note" aria-label="Who can open this page" style="all:initial;display:block;position:fixed;top:0;left:0;right:0;z-index:2147483647">` +
		`<template shadowrootmode="open"><style>` +
		`div{box-sizing:border-box;height:` + accessBarHeight + `;display:flex;align-items:center;justify-content:center;gap:10px;padding:0 16px;` +
		`background:` + level.color + `;color:#fff;font:600 13px/1 system-ui,-apple-system,"Segoe UI",sans-serif}` +
		`b{flex:none;font-weight:800;letter-spacing:.12em}` +
		`span{min-width:0;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}` +
		`</style><div><b>` + level.label + `</b><span title="` + text + `">` + text + `</span></div></template></spot-access-bar>` +
		`<style>:root{--spot-access-bar-height:` + accessBarHeight + `}` +
		`html{padding-top:var(--spot-access-bar-height)!important;scroll-padding-top:var(--spot-access-bar-height)}</style>`
}

// injectAccessBar puts the bar first in <body>, or where the parser would
// open the body when the page has no <body> tag.
func injectAccessBar(page []byte, markup string) []byte {
	lower := bytes.ToLower(page)
	at := -1
	if i := openTagEnd(lower, "<body"); i >= 0 {
		at = i
	} else if i := bytes.Index(lower, []byte("</head>")); i >= 0 {
		at = i + len("</head>")
	} else if i := openTagEnd(lower, "<html"); i >= 0 {
		at = i
	} else if i := openTagEnd(lower, "<!doctype"); i >= 0 {
		at = i
	} else {
		at = 0
	}
	out := make([]byte, 0, len(page)+len(markup))
	out = append(out, page[:at]...)
	out = append(out, markup...)
	return append(out, page[at:]...)
}

func openTagEnd(lower []byte, tag string) int {
	for from := 0; ; {
		i := bytes.Index(lower[from:], []byte(tag))
		if i < 0 {
			return -1
		}
		i += from
		next := i + len(tag)
		if next < len(lower) && strings.IndexByte(" \t\r\n/>", lower[next]) >= 0 {
			if end := bytes.IndexByte(lower[next:], '>'); end >= 0 {
				return next + end + 1
			}
			return -1
		}
		from = next
	}
}

func isHTMLPage(name string) bool {
	return strings.HasPrefix(contentTypeForName(name), "text/html")
}

// serveHTMLWithAccessBar serves an HTML page of the site with the bar for its
// current policy. The ETag covers the served bytes, so an access change is
// never answered with a 304 for a page that shows the old level. A typed
// viewer keeps its "<version>-<64 hex>" ETag shape through etagPrefix.
func (s *Server) serveHTMLWithAccessBar(w http.ResponseWriter, r *http.Request, site, name, etagPrefix string, body io.Reader) {
	policy, err := s.policyForSite(r.Context(), site)
	if err != nil {
		// authorizeSiteAccess has already refused unreadable policies.
		log.Printf("access bar %s: %v", site, err)
		httpError(w, http.StatusServiceUnavailable, "could not read the site's access policy")
		return
	}
	page, err := io.ReadAll(io.LimitReader(body, maxAccessBarPageSize+1))
	if err != nil {
		log.Printf("access bar %s/%s: %v", site, name, err)
		httpError(w, http.StatusInternalServerError, "could not read site file")
		return
	}
	w.Header().Set("Content-Type", contentTypeForName(name))
	if len(page) > maxAccessBarPageSize {
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, io.MultiReader(bytes.NewReader(page), body))
		}
		return
	}
	page = injectAccessBar(page, accessBarMarkup(accessBarFor(policy)))
	sum := sha256.Sum256(page)
	w.Header().Set("ETag", `"`+etagPrefix+hex.EncodeToString(sum[:])+`"`)
	w.Header().Del("Last-Modified")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(page))
}
