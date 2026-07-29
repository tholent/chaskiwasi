package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"
)

// assets holds every template and static file. Embedding them is what makes
// §9.1's "no build step" true: the binary is the whole UI, and there is no
// asset pipeline, no CDN, and nothing to fetch at runtime on a home LAN.
//
//go:embed templates static
var assets embed.FS

// pageFiles are the full-page templates, each rendered inside layout.html.
// The list is explicit rather than globbed so that adding a page is a visible
// edit here — and so a stray file in templates/pages/ cannot become a route's
// silent surprise.
var pageFiles = []string{
	"login.html",
	"dashboard.html",
	"contacts.html",
	"held.html",
	"changes.html",
	"settings.html",
	"account.html",
}

// fragmentFiles are rendered standalone, without the layout: htmx swaps them
// into an already-loaded page.
var fragmentFiles = []string{
	"device_status.html",
	"held_list.html",
}

// renderer holds one parsed template set per page. Per-page sets are what let
// every page define its own "content" block without the sets colliding, which
// is the one awkward corner of html/template's flat namespace.
type renderer struct {
	pages     map[string]*template.Template
	fragments map[string]*template.Template
}

var templateFuncs = template.FuncMap{
	"datetime": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2 Jan 2006, 15:04")
	},
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2 Jan 2006")
	},
	"ago": humanAgo,
}

func newRenderer() (*renderer, error) {
	r := &renderer{
		pages:     make(map[string]*template.Template, len(pageFiles)),
		fragments: make(map[string]*template.Template, len(fragmentFiles)),
	}

	for _, name := range pageFiles {
		t, err := template.New("layout.html").Funcs(templateFuncs).ParseFS(assets,
			"templates/layout.html",
			"templates/partials/*.html",
			"templates/fragments/*.html",
			"templates/pages/"+name,
		)
		if err != nil {
			return nil, fmt.Errorf("web: parsing page %s: %w", name, err)
		}
		r.pages[name] = t
	}

	for _, name := range fragmentFiles {
		t, err := template.New(name).Funcs(templateFuncs).ParseFS(assets, "templates/fragments/"+name)
		if err != nil {
			return nil, fmt.Errorf("web: parsing fragment %s: %w", name, err)
		}
		r.fragments[name] = t
	}
	return r, nil
}

// layout is the data every page shares.
type layout struct {
	Title    string
	Nav      string // which nav item to mark current
	Guardian string
	CSRF     string
	Flash    *flash
	// CertWarning is the §12.3 banner: persistent, on every page, when the
	// device-listener certificate has under 45 days left. Deliberately not an
	// INBOX notice — certificate operations are operator noise, not family
	// record, and the child's inbox is not an ops channel.
	CertWarning string
}

// newLayout builds the shared page data for a request under sess.
func (s *Server) newLayout(r *http.Request, sess session, title, nav string) layout {
	return layout{
		Title:       title,
		Nav:         nav,
		Guardian:    sess.Guardian,
		CSRF:        s.issueCSRF(sess),
		Flash:       flashFor(r),
		CertWarning: s.certWarning(),
	}
}

// page renders a full page. It renders into a buffer first so that a template
// error produces a clean 500 instead of a half-written page with a 200 already
// on the wire.
func (s *Server) page(w http.ResponseWriter, status int, name string, data any) {
	t, ok := s.tmpl.pages[name]
	if !ok {
		s.log.Error("web: no such page template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.write(w, status, t, name, data)
}

// fragment renders an htmx-swapped fragment.
func (s *Server) fragment(w http.ResponseWriter, name string, data any) {
	t, ok := s.tmpl.fragments[name]
	if !ok {
		s.log.Error("web: no such fragment template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.write(w, http.StatusOK, t, name, data)
}

func (s *Server) write(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.log.Error("web: rendering failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// This UI renders no third-party content and loads no third-party asset;
	// saying so lets the browser enforce it.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// staticHandler serves the embedded assets. The vendored htmx build and the
// stylesheet never change between deploys of a given binary, so they are safe
// to cache hard — and a home LAN with a slow uplink is exactly where that
// matters.
func staticHandler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic("web: static assets missing from the embedded FS: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fileServer.ServeHTTP(w, r)
	})
}

// humanAgo renders an elapsed time the way a guardian reads it: "4 minutes
// ago", not a duration.
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

// humanDuration renders a short wait for the login page.
func humanDuration(d time.Duration) string {
	secs := int(d.Seconds() + 0.999)
	if secs < 60 {
		return plural(secs, "second")
	}
	return plural((secs+59)/60, "minute")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
