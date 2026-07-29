package web

import (
	"net/http"
	"strconv"
	"strings"
)

// §9.1: settings that live in the human-owned wasi.toml are displayed
// READ-ONLY with the file path shown. Two writers to one file is the failure
// mode the ownership split exists to prevent, and the UI makes the boundary
// visible rather than silently omitting the fields — a guardian who cannot
// find a setting assumes it does not exist; a guardian who can see it and its
// file knows exactly what to edit and where.
//
// The labels here are guardian-facing English on purpose. The TOML keys behind
// them carry the internal vocabulary the §9.1 boundary keeps out of this UI
// (V-14), so this page shows what each setting *means*, and the file path for
// anyone who needs the key itself.
type settingRow struct {
	Label string
	Value string
	Note  string
}

type settingsGroup struct {
	Title string
	Rows  []settingRow
}

type settingsPage struct {
	layout
	ConfigPath string
	Groups     []settingsGroup
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	page := settingsPage{
		layout:     s.newLayout(r, sess, "Settings", "settings"),
		ConfigPath: s.configPath,
	}

	cfg := s.watcher.Current()
	if cfg == nil {
		s.page(w, http.StatusOK, "settings.html", page)
		return
	}

	page.Groups = []settingsGroup{
		{Title: "Household", Rows: []settingRow{
			{Label: "Name on the device", Value: cfg.Owner.Name,
				Note: "Used when the device has to invent a subject line for a letter."},
			{Label: "Mailbox address", Value: cfg.Mail.Address},
		}},
		{Title: "Letters", Rows: []settingRow{
			{Label: "Longest letter", Value: plural(cfg.Sync.MaxLetterChars, "character"),
				Note: "Applies in both directions. The full text always stays in the mailbox."},
			{Label: "Letters delivered after a factory reset", Value: strconv.Itoa(cfg.Sync.ResyncWindow)},
			{Label: "How often the device checks in", Value: humanInterval(cfg.Sync.IntervalS)},
			{Label: "Held folder", Value: cfg.Mail.HeldFolder,
				Note: "Where messages wait for review."},
		}},
		{Title: "Contacts", Rows: []settingRow{
			{Label: "Maximum contacts", Value: strconv.Itoa(cfg.Ayllu.MaxContacts),
				Note: "Removed contacts still count: their letters have to keep working."},
		}},
		{Title: "Notifications and records", Rows: []settingRow{
			{Label: "Quiet period between device alerts", Value: plural(cfg.Pututu.CoalesceMin, "minute")},
			{Label: "Device health kept for", Value: plural(cfg.Kipu.RetentionDays, "day")},
			{Label: "Backups kept for", Value: plural(cfg.Backup.RetainDays, "day")},
			{Label: "Copies of change notices sent to", Value: addressList(cfg.Guardian.CopyAddresses),
				Note: "System messages only. A child's letters are never copied anywhere."},
		}},
		{Title: "Network", Rows: []settingRow{
			{Label: "This web interface listens on", Value: cfg.Guardian.Listen,
				Note: "Intended for your home network or a VPN. Exposing it to the open internet is at your own risk."},
			{Label: "The device connects to", Value: cfg.Device.Listen},
			{Label: "SMS provider", Value: orNone(cfg.Carrier.Name)},
		}},
	}

	s.page(w, http.StatusOK, "settings.html", page)
}

func humanInterval(seconds int) string {
	switch {
	case seconds <= 0:
		return "—"
	case seconds%3600 == 0:
		return plural(seconds/3600, "hour")
	case seconds%60 == 0:
		return plural(seconds/60, "minute")
	default:
		return plural(seconds, "second")
	}
}

// addressList renders guardian.copy_addresses. These are guardian addresses
// from human-owned config, not contact addresses, and showing them on the
// page a guardian consults to check that configuration is the point of it.
func addressList(addrs []string) string {
	if len(addrs) == 0 {
		return "nobody"
	}
	return strings.Join(addrs, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none configured"
	}
	return s
}
