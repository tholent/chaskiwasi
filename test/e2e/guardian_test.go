//go:build e2e

package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// guardianUI is a browser, near enough. It exists because V-7 and V-18 are
// about the guardian *flows*, not about the packages underneath them: the
// release paths in particular are chosen from a fresh server-side resolution
// (internal/web.releaseFor), so driving filing directly would skip the one
// decision the test is checking.
//
// Everything §9.2 requires of a real browser is honoured rather than worked
// around. Unsafe requests carry Origin and Sec-Fetch-Site: same-origin,
// because the server refuses them otherwise (internal/web.checkOrigin) — that
// check is a deliberate defense and a suite that disabled it to make itself
// easier to write would delete the only thing protecting login from CSRF.
type guardianUI struct {
	base   string
	client *http.Client
}

func newGuardianUI(t testing.TB, s *stack) *guardianUI {
	t.Helper()

	// §12.1's guardian listener carries a public certificate in production and
	// a self-signed one here. Pin it rather than skipping verification: a
	// suite that runs with verification off cannot notice a listener serving
	// the wrong identity, and there is never a reason for this one to.
	pem := s.readVolumeFile(t, s.volumeFor(t, "wasi", "/config/tls"), "guardian.crt")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("guardian.crt from the wasi TLS volume is not a PEM certificate")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}

	return &guardianUI{
		base: guardianBaseURL,
		client: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
			// Follow redirects; every mutation answers 303 to a page whose
			// flash message is how the UI reports the outcome.
		},
	}
}

// get fetches a page and returns its body and status.
func (g *guardianUI) get(t testing.TB, path string) (string, int) {
	t.Helper()
	resp, err := g.client.Get(g.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading GET %s: %v", path, err)
	}
	return string(body), resp.StatusCode
}

// page fetches a page that must render, and returns its HTML.
func (g *guardianUI) page(t testing.TB, path string) string {
	t.Helper()
	body, status := g.get(t, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s: status %d\n%s", path, status, body)
	}
	return body
}

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// csrfFrom pulls a token out of rendered HTML. Any page's token authorises any
// form: internal/web binds it to the session and a nonce, not to a route.
func csrfFrom(t testing.TB, html string) string {
	t.Helper()
	match := csrfField.FindStringSubmatch(html)
	if match == nil {
		t.Fatal("no csrf_token field in the rendered page")
	}
	return match[1]
}

// postRaw submits a form the way a browser would, without adding a CSRF token.
// Login uses it (there is no session to bind a token to); everything else goes
// through submit.
func (g *guardianUI) postRaw(t testing.TB, path string, form url.Values) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("building POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", g.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading POST %s: %v", path, err)
	}
	return string(body), resp.StatusCode
}

// submit renders formPage, takes its CSRF token, and posts action. Rendering
// the page first is not ceremony: it is how a guardian gets a token, and it
// exercises the live IMAP read behind /held on the way past.
func (g *guardianUI) submit(t testing.TB, formPage, action string, form url.Values) string {
	t.Helper()
	form.Set("csrf_token", csrfFrom(t, g.page(t, formPage)))
	body, status := g.postRaw(t, action, form)
	if status != http.StatusOK {
		t.Fatalf("POST %s: status %d\n%s", action, status, body)
	}
	return body
}

// csrfToken renders a page and returns a token from it, on the calling
// goroutine. It exists for the crash tests, which must obtain a token before
// the process they are about to kill goes away.
func (g *guardianUI) csrfToken(t testing.TB, formPage string) string {
	t.Helper()
	return csrfFrom(t, g.page(t, formPage))
}

// tryPost submits a form and returns an error instead of failing a test.
//
// The crash cases post from a goroutine and then SIGKILL the server, so the
// request may be answered, refused, or cut off mid-flight — none of which is a
// failure, and all of which t.Fatalf would turn into one (or, from a
// non-test goroutine, into an abandoned test binary).
func (g *guardianUI) tryPost(path string, form url.Values) error {
	req, err := http.NewRequest(http.MethodPost, g.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building POST %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", g.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("draining POST %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// login signs in and fails if the session did not take.
func (g *guardianUI) login(t testing.TB, name, password string) {
	t.Helper()
	// §9.2: the field is `guardian`, not `name`.
	body, status := g.postRaw(t, "/login", url.Values{
		"guardian": {name},
		"password": {password},
	})
	if status != http.StatusOK {
		t.Fatalf("login as %q: status %d\n%s", name, status, body)
	}
	if strings.Contains(body, `name="password"`) {
		t.Fatalf("login as %q landed back on the sign-in form", name)
	}
}

// loginAttempt signs in and reports the status, without asserting success.
// V-19 needs the failures.
func (g *guardianUI) loginAttempt(t testing.TB, name, password string) (string, int) {
	t.Helper()
	return g.postRaw(t, "/login", url.Values{"guardian": {name}, "password": {password}})
}

func (g *guardianUI) addContact(t testing.TB, name, address string) {
	t.Helper()
	g.submit(t, "/contacts", "/contacts/add", url.Values{"name": {name}, "address": {address}})
}

func (g *guardianUI) deactivate(t testing.TB, contactID string) {
	t.Helper()
	g.submit(t, "/contacts", "/contacts/deactivate", url.Values{"contact_id": {contactID}})
}

func (g *guardianUI) readdress(t testing.TB, contactID, address string) {
	t.Helper()
	g.submit(t, "/contacts", "/contacts/readdress", url.Values{"contact_id": {contactID}, "address": {address}})
}

// heldRow is one row of the Held review as the UI actually renders it. Kind is
// read back from which release control the page offered, because that — not
// anything the test computes — is the flow a guardian would be given (§8).
type heldRow struct {
	UID  uint32
	From string
	Kind string // "stranger" | "deactivated" | "known" | "unreadable"
}

var (
	heldItem    = regexp.MustCompile(`(?s)<li>(.*?)</li>`)
	heldUID     = regexp.MustCompile(`name="uid" value="(\d+)"`)
	heldFrom    = regexp.MustCompile(`<code>([^<]*)</code>`)
	heldButtons = map[string]string{
		"Add as contact, then deliver":  "stranger",
		"Restore contact, then deliver": "deactivated",
	}
)

// heldRows renders /held and reads back what it offers.
func (g *guardianUI) heldRows(t testing.TB) []heldRow {
	t.Helper()
	html := g.page(t, "/held")

	var rows []heldRow
	for _, item := range heldItem.FindAllStringSubmatch(html, -1) {
		block := item[1]
		row := heldRow{Kind: "unreadable"}
		if m := heldUID.FindStringSubmatch(block); m != nil {
			uid, err := strconv.ParseUint(m[1], 10, 32)
			if err != nil {
				t.Fatalf("parsing held uid %q: %v", m[1], err)
			}
			row.UID = uint32(uid)
		}
		if m := heldFrom.FindStringSubmatch(block); m != nil {
			row.From = m[1]
		}
		for label, kind := range heldButtons {
			if strings.Contains(block, label) {
				row.Kind = kind
			}
		}
		if row.Kind == "unreadable" && strings.Contains(block, ">Deliver</button>") {
			row.Kind = "known"
		}
		rows = append(rows, row)
	}
	return rows
}

// heldRowFrom returns the single Held row sent by addr, or fails.
func (g *guardianUI) heldRowFrom(t testing.TB, addr string) heldRow {
	t.Helper()
	var found []heldRow
	for _, row := range g.heldRows(t) {
		if strings.EqualFold(row.From, addr) {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one held message from %s, got %d: %+v", addr, len(found), g.heldRows(t))
	}
	return found[0]
}

// release performs a release. name is the contact name for the stranger flow
// and ignored by the others, exactly as the form is.
func (g *guardianUI) release(t testing.TB, uid uint32, name string) string {
	t.Helper()
	form := url.Values{"uid": {strconv.FormatUint(uint64(uid), 10)}}
	if name != "" {
		form.Set("name", name)
	}
	return g.submit(t, "/held", "/held/release", form)
}

// changePassword performs a self-service password change (§9.2). The epoch
// bump it triggers is what V-19 is about.
func (g *guardianUI) changePassword(t testing.TB, current, next string) {
	t.Helper()
	g.submit(t, "/account", "/account/password", url.Values{
		"current_password": {current},
		"new_password":     {next},
		"confirm_password": {next},
	})
}

// pageURLs are the guardian surfaces V-14 renders and greps. Kept as a list so
// a new page added without a thought for §0.1's vocabulary boundary shows up
// here as a gap rather than passing silently.
var pageURLs = []string{"/", "/contacts", "/held", "/changes", "/settings", "/account"}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... (%d bytes total)", len(s))
}
