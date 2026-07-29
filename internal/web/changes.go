package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

// §7.4: the change log holds full detail including addresses, and §9.1
// surfaces it here. This is the second and last place an address is shown to a
// guardian — the file is never device-visible, which is exactly why I-2
// permits it, and it is where §7.4 tells guardians to look for the old and new
// address behind an address change (the notice letter deliberately carries
// neither).
const changeLogLimit = 200

type changeView struct {
	At         time.Time
	Actor      string
	Action     string
	ContactID  string
	Name       string
	OldAddress string
	NewAddress string
}

type changesPage struct {
	layout
	Changes []changeView
	Err     string
	// Truncated tells the guardian they are seeing a window, not the file.
	Truncated bool
	Limit     int
}

func (s *Server) handleChangeLog(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)

	page := changesPage{
		layout: s.newLayout(r, sess, "Changes", "changes"),
		Limit:  changeLogLimit,
	}
	events, truncated, err := s.readChangeLog()
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No log file yet is the normal state of a new deployment, not an
		// error worth showing as one.
	case err != nil:
		s.log.Error("web: reading the change log failed", "error", err)
		page.Err = "The change log could not be read."
	}

	page.Truncated = truncated
	page.Changes = make([]changeView, 0, len(events))
	// Newest first: the question a guardian brings to this page is almost
	// always "what just happened".
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		page.Changes = append(page.Changes, changeView{
			At:         e.At,
			Actor:      e.Actor,
			Action:     string(e.Action),
			ContactID:  e.ContactID,
			Name:       e.Name,
			OldAddress: e.OldAddress,
			NewAddress: e.NewAddress,
		})
	}

	s.page(w, http.StatusOK, "changes.html", page)
}

// readChangeLog reads the tail of ayllu-log.jsonl. It streams rather than
// slurping because the file is append-only and unbounded by design; the ring
// keeps memory flat regardless of how many years of history the family has.
//
// A line that will not parse is skipped rather than fatal: the log is an
// accountability record, and showing the 199 lines that are readable beats
// showing none because one is not.
func (s *Server) readChangeLog() ([]ayllu.Event, bool, error) {
	if s.dataDir == "" {
		return nil, false, nil
	}
	f, err := os.Open(filepath.Join(s.dataDir, "ayllu-log.jsonl"))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	ring := make([]ayllu.Event, 0, changeLogLimit)
	truncated := false

	scanner := bufio.NewScanner(f)
	// A change-log line is a handful of fields; the default 64 KiB token limit
	// is already far more than one can be.
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ayllu.Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if len(ring) == changeLogLimit {
			ring = ring[1:]
			truncated = true
		}
		ring = append(ring, e)
	}
	if err := scanner.Err(); err != nil {
		return ring, truncated, err
	}
	return ring, truncated, nil
}
