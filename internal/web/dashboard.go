package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// deviceStatus is §9.1's device panel: last sync, battery, signal, read from
// the newest health line. Nothing here is content and nothing here is an
// address; it is the answer to "is the thing in her bag still alive".
type deviceStatus struct {
	LastSync   time.Time
	HealthAt   time.Time
	HaveHealth bool

	BatteryPct  int
	HaveBattery bool

	SignalDBm  int
	SignalWord string
	HaveSignal bool

	Network  string
	Firmware string
}

// deliveryView is one row of the recent-deliveries panel: an id, a status, and
// a timestamp. Never content, at any level (I-1) — the panel exists so a
// guardian can answer "did that letter go" without anyone reading the letter.
type deliveryView struct {
	ID     string
	Status string
	At     time.Time
}

// balancePanel is the carrier credit panel (§10.4). Show is false both when no
// carrier is wired and when the provider returns ErrUnsupported: an optional
// capability degrades, it does not error.
type balancePanel struct {
	Show     bool
	Amount   float64
	Currency string
	Err      string
}

type dashboardPage struct {
	layout
	Device     deviceStatus
	Deliveries []deliveryView
	Balance    balancePanel
	OwnerName  string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	page := dashboardPage{
		layout:     s.newLayout(r, sess, "Home", "home"),
		Device:     s.deviceStatus(),
		Deliveries: s.recentDeliveries(),
		Balance:    s.balancePanel(r.Context()),
	}
	if cfg := s.watcher.Current(); cfg != nil {
		page.OwnerName = cfg.Owner.Name
	}
	s.page(w, http.StatusOK, "dashboard.html", page)
}

// handleDeviceStatusFragment serves the htmx poll that refreshes the device
// panel in place.
func (s *Server) handleDeviceStatusFragment(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, "device_status.html", s.deviceStatus())
}

// deviceStatus assembles the panel from two sources: state.json for the last
// sync time, and the newest health day-file for battery and signal.
func (s *Server) deviceStatus() deviceStatus {
	var st deviceStatus
	if s.state != nil {
		st.LastSync = s.state.Snapshot().LastSyncAt
	}

	line, ok := s.newestHealthLine()
	if !ok {
		return st
	}
	st.HaveHealth, st.HealthAt = true, line.At

	if v, ok := numberField(line.Fields, "battery_pct"); ok {
		st.BatteryPct, st.HaveBattery = int(v), true
	}
	if v, ok := numberField(line.Fields, "rssi"); ok {
		st.SignalDBm, st.HaveSignal = int(v), true
		st.SignalWord = signalWord(int(v))
	}
	st.Network, _ = line.Fields["rat"].(string)
	st.Firmware, _ = line.Fields["fw"].(string)
	return st
}

// healthLine is one line of a device health day-file.
//
// The file format belongs to internal/kipu, which writes it but exposes no
// reader — this package needs one and does not own that one, so it parses the
// two fields it needs and ignores everything else. The firmware may add fields
// at any time (§4.8 round-trips unknown ones untouched), so an unknown field
// here is normal, not an error.
type healthLine struct {
	At     time.Time      `json:"at"`
	Fields map[string]any `json:"kipu"`
}

// newestHealthLine returns the most recent line of the most recent day-file.
// A missing directory is normal: a device that has never synced has no health
// history, and the panel says "no report yet" rather than failing.
func (s *Server) newestHealthLine() (healthLine, bool) {
	if s.dataDir == "" {
		return healthLine{}, false
	}
	dir := filepath.Join(s.dataDir, "kipu")

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("web: reading the device health directory failed", "error", err)
		}
		return healthLine{}, false
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return healthLine{}, false
	}
	// Day-files are named YYYY-MM-DD.jsonl, so lexical order is date order.
	sort.Strings(names)

	data, err := os.ReadFile(filepath.Join(dir, names[len(names)-1]))
	if err != nil {
		s.log.Warn("web: reading the newest device health file failed", "error", err)
		return healthLine{}, false
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		var hl healthLine
		if err := json.Unmarshal([]byte(lines[i]), &hl); err != nil {
			// A single unreadable line is not worth failing the panel over;
			// keep walking backwards for the newest one that parses.
			continue
		}
		return hl, true
	}
	return healthLine{}, false
}

// numberField reads a JSON number out of the health block. Everything arrives
// as float64 through encoding/json's any decoding, whatever the firmware sent.
func numberField(fields map[string]any, key string) (float64, bool) {
	v, ok := fields[key].(float64)
	return v, ok
}

// signalWord turns an RSSI in dBm into something a guardian can act on. The
// boundaries are the usual cellular rules of thumb; the point is only to
// distinguish "fine", "marginal", and "she is somewhere with no coverage".
func signalWord(dbm int) string {
	switch {
	case dbm >= -85:
		return "good"
	case dbm >= -100:
		return "fair"
	default:
		return "weak"
	}
}

// recentDeliveriesLimit bounds the panel. The underlying ring holds thousands;
// a guardian wants the last screenful.
const recentDeliveriesLimit = 25

// recentDeliveries reads the outbound ack ring: ids, statuses, and timestamps
// only, never content (I-1, §9.1).
func (s *Server) recentDeliveries() []deliveryView {
	if s.state == nil {
		return nil
	}
	entries := s.state.Snapshot().Acks.Entries

	out := make([]deliveryView, 0, recentDeliveriesLimit)
	for i := len(entries) - 1; i >= 0 && len(out) < recentDeliveriesLimit; i-- {
		out = append(out, deliveryView{
			ID:     entries[i].LocalID,
			Status: strings.ReplaceAll(entries[i].Status, "_", " "),
			At:     entries[i].At,
		})
	}
	return out
}

// balancePanelTimeout bounds the provider call. The dashboard must render even
// when the carrier's API is slow or down; a credit figure is not worth a
// hanging page.
const balancePanelTimeout = 3 * time.Second

// balancePanel reads remaining SMS credit through the BalanceReporter seam
// (§10.4). ErrUnsupported hides the panel rather than erroring — a provider
// without the concept is a normal configuration, not a fault.
func (s *Server) balancePanel(ctx context.Context) balancePanel {
	if s.balance == nil {
		return balancePanel{}
	}

	ctx, cancel := context.WithTimeout(ctx, balancePanelTimeout)
	defer cancel()

	b, err := s.balance.Balance(ctx)
	switch {
	case errors.Is(err, ErrBalanceUnsupported):
		return balancePanel{}
	case err != nil:
		s.log.Warn("web: reading the carrier balance failed", "error", err)
		return balancePanel{Show: true, Err: "The provider could not be reached."}
	}
	return balancePanel{Show: true, Amount: b.Amount, Currency: b.Currency}
}

// String renders a balance the way the panel wants it, keeping the formatting
// decision out of the template.
func (b balancePanel) String() string {
	if b.Currency == "" {
		return fmt.Sprintf("%.2f", b.Amount)
	}
	return fmt.Sprintf("%.2f %s", b.Amount, b.Currency)
}
