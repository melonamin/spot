package main

import (
	"html/template"
	"log"
	"net/http"
)

// statusPage is a plain, brand-neutral page for browser visitors in delegated
// login mode, where Spot sits behind another product and its own look would
// be out of place.
type statusPage struct {
	Title   string
	Message string
	Detail  string
	Links   []statusPageLink
}

type statusPageLink struct {
	Label   string
	URL     string
	NewTab  bool
	Primary bool
}

var statusPageTemplate = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root{--bg:#f6f7f9;--card:#fff;--ink:#16181d;--muted:#5d6470;--line:#e3e6eb;--accent:#16181d;--on-accent:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a20;--ink:#eef0f3;--muted:#9aa2ae;--line:#2a2f38;--accent:#eef0f3;--on-accent:#0f1115}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:1.5rem;background:var(--bg);color:var(--ink);font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
main{width:100%;max-width:26rem;background:var(--card);border:1px solid var(--line);border-radius:14px;padding:2rem}
.mark{width:40px;height:40px;border-radius:10px;display:grid;place-items:center;background:var(--bg);border:1px solid var(--line);margin-bottom:1.25rem}
h1{font-size:1.2rem;line-height:1.3;margin:0 0 .5rem}
p{margin:0 0 .75rem;color:var(--muted)}
.detail{font-size:.875rem;padding:.6rem .75rem;border-radius:8px;background:var(--bg);border:1px solid var(--line);color:var(--ink);overflow-wrap:anywhere}
.actions{display:flex;flex-wrap:wrap;gap:.5rem;margin-top:1.5rem}
a.btn{display:inline-block;padding:.5rem .9rem;border-radius:8px;border:1px solid var(--line);color:var(--ink);text-decoration:none;font-weight:500}
a.btn.primary{background:var(--accent);border-color:var(--accent);color:var(--on-accent)}
a.btn:focus-visible{outline:2px solid var(--ink);outline-offset:2px}
</style>
</head>
<body>
<main>
<div class="mark" aria-hidden="true"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg></div>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .Detail}}<p class="detail">{{.Detail}}</p>{{end}}
{{if .Links}}<div class="actions">{{range .Links}}<a class="btn{{if .Primary}} primary{{end}}" href="{{.URL}}"{{if .NewTab}} target="_blank" rel="noopener"{{end}}>{{.Label}}</a>{{end}}</div>{{end}}
</main>
</body>
</html>
`))

func writeStatusPage(w http.ResponseWriter, status int, page statusPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := statusPageTemplate.Execute(w, page); err != nil {
		log.Printf("status page: %v", err)
	}
}

// sitesLink points browser visitors back to the product that fronts Spot.
func (s *Server) sitesLink() []statusPageLink {
	if s.apexRedirectURL == "" {
		return nil
	}
	return []statusPageLink{{Label: "Go to Sites", URL: s.apexRedirectURL, Primary: true}}
}

// denySiteAccess answers a refused site request: a status page for a browser
// page load in delegated login mode, JSON for everything else.
func (s *Server) denySiteAccess(w http.ResponseWriter, r *http.Request, status int, apiMessage string, page statusPage) {
	if s.login != nil && isNavigation(r) {
		if page.Links == nil {
			page.Links = s.sitesLink()
		}
		writeStatusPage(w, status, page)
		return
	}
	httpError(w, status, apiMessage)
}
