package web

import (
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"
)

// forbiddenWords are the three internal identifiers of the §9.1 vocabulary
// boundary. They are greppable on purpose and belong in Go code and internal
// logs; a guardian reads "Contacts", "Held Messages", and "Device health".
var forbiddenWords = []string{"pututu", "ayllu", "kipu"}

// TestV14_TemplateSourcesUseNoInternalVocabulary is the static half of V-14:
// nothing under templates/ (or in the served static assets) may contain one of
// the three words, in any case.
func TestV14_TemplateSourcesUseNoInternalVocabulary(t *testing.T) {
	err := fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// VENDOR.md documents where htmx came from and is not served to a
		// browser; everything else in the embedded FS reaches one.
		if path.Base(p) == "VENDOR.md" {
			return nil
		}
		data, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(data))
		for _, word := range forbiddenWords {
			if strings.Contains(lower, word) {
				t.Errorf("%s contains the internal word %q (§9.1, V-14)", p, word)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded assets: %v", err)
	}
}

// TestV14_RenderedPagesUseNoInternalVocabulary is the half that actually
// matters: a template can be clean while a handler feeds it a label, a path,
// or an error string carrying one of the words. Every page and fragment is
// rendered with data exercising its branches, and the HTML itself is grepped.
func TestV14_RenderedPagesUseNoInternalVocabulary(t *testing.T) {
	h := newHarness(t)
	h.addGuardian("dad")
	cookie := h.login("dad", testPassword)
	seedEverySurface(t, h, cookie)

	pages := []string{
		"/", "/contacts", "/held", "/changes", "/settings", "/account",
		"/held/list", "/status/device",
		// Flash-bearing variants: the outcome text is server-side, so it is
		// rendered content too.
		"/contacts?m=contacts-full",
		"/contacts?m=contact-added",
		"/held?m=release-failed",
		"/account?m=password-changed",
	}
	for _, p := range pages {
		rec := h.get(p, cookie)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d, want 200", p, rec.Code)
		}
		assertNoInternalVocabulary(t, p, rec.Body.String())
	}

	// The login page renders without a session and has its own error strings.
	assertNoInternalVocabulary(t, "/login", h.get("/login", nil).Body.String())
}

func assertNoInternalVocabulary(t *testing.T, where, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, word := range forbiddenWords {
		if idx := strings.Index(lower, word); idx >= 0 {
			start := max(idx-60, 0)
			end := min(idx+60, len(body))
			t.Errorf("%s renders the internal word %q (§9.1, V-14):\n…%s…", where, word, body[start:end])
		}
	}
}

// TestV14_FlashTextIsGuardianFacing greps the flash table directly, since a
// message added there is rendered on a page long before anyone remembers to
// add that page to the list above.
func TestV14_FlashTextIsGuardianFacing(t *testing.T) {
	for code, f := range flashes {
		lower := strings.ToLower(f.Text)
		for _, word := range forbiddenWords {
			if strings.Contains(lower, word) {
				t.Errorf("flash %q contains the internal word %q", code, word)
			}
		}
	}
}

// seedEverySurface fills the harness with data that makes each page render its
// populated branch rather than its empty one — an empty table cannot leak a
// label.
func seedEverySurface(t *testing.T, h *harness, cookie *http.Cookie) {
	t.Helper()

	// Contacts: one active, one tombstone with a past address.
	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Rosa"}, "address": {"rosa@example.test"},
	}, cookie), "/contacts?m=contact-added")
	_, contacts := h.ayllu.List()
	rosaID := contacts[0].ID
	mustRedirect(t, h.post("/contacts/readdress", "/contacts", url.Values{
		"contact_id": {rosaID}, "address": {"rosa2@example.test"},
	}, cookie), "/contacts?m=contact-address")
	mustRedirect(t, h.post("/contacts/deactivate", "/contacts", url.Values{
		"contact_id": {rosaID},
	}, cookie), "/contacts?m=contact-off")
	mustRedirect(t, h.post("/contacts/add", "/contacts", url.Values{
		"name": {"Tio"}, "address": {"tio@example.test"},
	}, cookie), "/contacts?m=contact-added")

	// Held: one of each classification.
	at := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	h.mailbox.put(heldFolder, letter("new@example.test", "hello", "hi"), at)
	h.mailbox.put(heldFolder, letter("rosa2@example.test", "later", "hi"), at.Add(time.Hour))
	h.mailbox.put(heldFolder, letter("tio@example.test", "earlier", "hi"), at.Add(2*time.Hour))
	h.mailbox.put(heldFolder, []byte("not a message\r\n"), at.Add(3*time.Hour))

	// Deliveries and last sync.
	h.seedDeliveries()

	// A device health day-file with the newest line last.
	h.seedHealth(`{"at":"2026-07-29T11:30:00Z","` + healthBlockKey +
		`":{"battery_pct":84,"rat":"ltem","rssi":-97,"fw":"0.3.1"}}`)

	// A carrier balance panel.
	h.server.balance = fixedBalance{value: Balance{Amount: 4.25, Currency: "USD"}}

	// A certificate inside its warning window.
	h.pinCertWarning("The device connection certificate expires in 12 days, on 10 August 2026. Renewing it is a manual step.")
}
