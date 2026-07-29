// Package syncsvc implements POST /sync — the entire device<->server surface
// (wasi-server-plan §4). The device initiates every exchange; the server never
// contacts the device except through the pututu doorbell (§10), which carries
// no information beyond "sync now".
//
// Three properties shape everything here and are easy to break by accident:
//
//   - Retrying an identical request is always safe (§4.1). Inbound is
//     dedup-keyed by letter id on the device (§4.5), outbound by the ack ring
//     (§4.7). There are no partial-success HTTP codes; partial outcomes live
//     in the acks array.
//   - The request cursor is authoritative (§4.4). The mirror in state.json
//     exists for the pututu skip check and operator display and never
//     overrides what the device sent.
//   - Nothing here persists letter content, and nothing here logs a body or a
//     subject at any level (I-1). Letter ids are logged where correlation is
//     needed.
package syncsvc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/derive"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/internal/state"
)

// RetryAfterSeconds is the Retry-After hint sent with 503 (§4.1). It is short
// relative to sync.interval_s because a 503 means a transient upstream outage
// the device should ride out, not a reason to skip a whole sync period — but
// long enough that a device on a per-MB link is not retrying in a tight loop.
const RetryAfterSeconds = 300

// fetchBatch bounds how many messages one sync pulls from the mailbox before
// assembly. Assembly is byte-budgeted anyway (§4.6), so this only has to be
// comfortably larger than the number of letters that can fit one response;
// anything left over is reported as more: true and drained next round.
const fetchBatch = 32

// ConfigSource supplies the current wasi.toml. It is an interface, not a
// *config.Config, because the file is hot-reloaded (§13): the handler must
// read the live value per request rather than capture one at construction.
// *config.Watcher satisfies it.
type ConfigSource interface {
	Current() *config.Config
}

// AylluStore is the slice of the contact list this handler needs: id lookup
// for outbound resolution (§4.7) and the device view for the response block
// (§4.3). DeviceView excludes addresses (I-2) and includes tombstones (§7.2).
type AylluStore interface {
	ByID(id string) (ayllu.Contact, bool)
	DeviceView(requestVersion int) *protocol.Ayllu
}

// KipuLog accepts one sync's device-health block (§4.8). Implementations own
// the protocol.MaxKipuBytes cap: a block too large to store is dropped there,
// never here, and never fails the sync.
type KipuLog interface {
	Append(block map[string]any, at time.Time) error
}

// Reconciler quarantines anything in INBOX whose sender does not resolve,
// before derivation can let the cursor pass it by (§5.1, test V-15).
//
// It is an interface here, and optional, because filing owns the behaviour and
// the sync path only owns the "at the top of every sync" half of §5.1's "at
// startup and at the top of every sync".
type Reconciler interface {
	Reconcile(ctx context.Context) error
}

// Deps are the handler's collaborators. Everything is injected so the whole of
// §4 is testable without a network, a mailbox, or a real clock.
type Deps struct {
	Config    ConfigSource
	Ayllu     AylluStore
	State     state.Store
	Mailbox   mailbox.Mailbox
	Submitter mailbox.Submitter
	Deriver   derive.Deriver
	Kipu      KipuLog

	// Reconciler is optional; when nil, §5.1's per-sync reconciliation pass is
	// skipped and only filing's own startup and IDLE paths run.
	Reconciler Reconciler

	// Now defaults to time.Now. The device has no RTC and takes server_time
	// from the response (§4.3), so this is the clock the device ends up on.
	Now func() time.Time

	// Logger defaults to slog.Default. No body or subject is ever passed to it
	// (I-1).
	Logger *slog.Logger
}

// Handler serves POST /sync (§4). It is safe for concurrent use; the device
// syncs one request at a time, but nothing here assumes that.
type Handler struct {
	deps Deps
}

var _ http.Handler = (*Handler)(nil)

// New validates deps and returns the handler. Every collaborator except
// Reconciler is required: a sync that cannot read config, resolve a contact,
// persist an ack, reach the mailbox, or derive a letter has nothing to
// usefully degrade to, and failing at construction beats failing per request.
func New(deps Deps) (*Handler, error) {
	var missing []string
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Config != nil, "Config")
	require(deps.Ayllu != nil, "Ayllu")
	require(deps.State != nil, "State")
	require(deps.Mailbox != nil, "Mailbox")
	require(deps.Submitter != nil, "Submitter")
	require(deps.Deriver != nil, "Deriver")
	require(deps.Kipu != nil, "Kipu")
	if len(missing) > 0 {
		return nil, fmt.Errorf("syncsvc: missing dependencies: %s", strings.Join(missing, ", "))
	}

	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// statusError carries an HTTP status out of the sync path. The status set is
// deliberately coarse (§4.1): 200 processed, 401 bad token, 503 + Retry-After
// when an upstream is unreachable, anything else transient.
type statusError struct {
	code       int
	retryAfter int
	err        error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

// unreachableError maps an upstream failure to 503 + Retry-After when it is a
// transport failure (§4.1), and to a plain transient 500 otherwise. The
// distinction matters to the device: 503 means "the same request will work
// later", which is exactly what an unreachable IMAP or SMTP host means.
func unreachableError(err error) *statusError {
	if errors.Is(err, mailbox.ErrUnreachable) {
		return &statusError{code: http.StatusServiceUnavailable, retryAfter: RetryAfterSeconds, err: err}
	}
	return &statusError{code: http.StatusInternalServerError, err: err}
}

// ServeHTTP implements §4.1's transport rules: POST only, bearer auth against
// the token hash, a 64 KB request cap, and the coarse status set.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := h.deps.Config.Current()

	if !authorized(r.Header.Get("Authorization"), cfg.Device.TokenHash) {
		// Never log the presented token, not even truncated: it is a bearer
		// credential, and a near-miss is as sensitive as a hit.
		h.deps.Logger.Warn("sync: rejected request with bad or missing bearer token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	req, err := decodeRequest(w, r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.deps.Logger.Warn("sync: request over size cap", "cap_bytes", protocol.MaxRequestBytes)
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		// The error text can quote the offending JSON, which could contain a
		// letter body (I-1), so it is neither logged nor returned.
		h.deps.Logger.Warn("sync: malformed request body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	resp, err := h.sync(r.Context(), cfg, req)
	if err != nil {
		var se *statusError
		if !errors.As(err, &se) {
			se = &statusError{code: http.StatusInternalServerError, err: err}
		}
		h.deps.Logger.Error("sync: failed", "status", se.code, "error", se.err)
		if se.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(se.retryAfter))
		}
		http.Error(w, http.StatusText(se.code), se.code)
		return
	}

	body, merr := json.Marshal(resp)
	if merr != nil {
		h.deps.Logger.Error("sync: marshalling response failed", "error", merr)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(body); werr != nil {
		// The device dropped the connection. Everything durable (acks, the
		// state mirror) is already written, and a retry of the identical
		// request replays it safely (§4.1), so this is a log line, not a
		// failure to repair.
		h.deps.Logger.Warn("sync: writing response failed", "error", werr)
	}
}

// authorized compares SHA-256 of the presented bearer token against
// device.token_hash in constant time (§4.1, §12.4).
func authorized(header, tokenHash string) bool {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	token := strings.TrimSpace(header[len(scheme):])

	want, err := hex.DecodeString(tokenHash)
	if err != nil || len(want) != sha256.Size {
		// config.Load validates the hash is 64 hex characters, so this is only
		// reachable via a hand-edit that hot-reload rejected — in which case
		// the last good config is still in force and this cannot happen. Deny
		// rather than allow if it ever does.
		return false
	}

	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// request is one decoded sync request. Outbound letters are held as decoded
// items rather than protocol.Outbound values so that an unknown field on a
// single letter can make that letter invalid (§4.7 step 2) without failing the
// whole sync — the outbox of a device running newer firmware still drains.
type request struct {
	Cursor            string
	AylluVersion      int
	PututuCounterSeen uint64
	Kipu              map[string]any
	Outbound          []outboundItem
}

// wireRequest mirrors protocol.Request with outbound letters left unparsed.
type wireRequest struct {
	Cursor            string            `json:"cursor"`
	AylluVersion      int               `json:"ayllu_version"`
	PututuCounterSeen uint64            `json:"pututu_counter_seen"`
	Kipu              map[string]any    `json:"kipu"`
	Outbound          []json.RawMessage `json:"outbound"`
}

// decodeRequest reads the body under the §4.1 size cap and decodes it.
func decodeRequest(w http.ResponseWriter, r *http.Request) (request, error) {
	body := http.MaxBytesReader(w, r.Body, protocol.MaxRequestBytes)

	var wire wireRequest
	if err := json.NewDecoder(body).Decode(&wire); err != nil {
		return request{}, err
	}

	req := request{
		Cursor:            wire.Cursor,
		AylluVersion:      wire.AylluVersion,
		PututuCounterSeen: wire.PututuCounterSeen,
		Kipu:              wire.Kipu,
	}
	for _, raw := range wire.Outbound {
		item, err := decodeOutbound(raw)
		if err != nil {
			return request{}, err
		}
		req.Outbound = append(req.Outbound, item)
	}
	return req, nil
}

// sync runs one whole exchange. The order matters: reconcile before deriving
// so nothing from a stranger can slip past the cursor (§5.1); process outbound
// before assembling the response so this round's acks ship with it; assemble
// letters last because that is what the byte budget is spent on.
func (h *Handler) sync(ctx context.Context, cfg *config.Config, req request) (*protocol.Response, error) {
	now := h.deps.Now().UTC()

	if h.deps.Reconciler != nil {
		if err := h.deps.Reconciler.Reconcile(ctx); err != nil {
			return nil, unreachableError(fmt.Errorf("syncsvc: reconcile: %w", err))
		}
	}

	// A kipu problem never fails a sync (§4.8): telemetry from a buggy device
	// is not a reason to lose a letter. The block is passed through untouched,
	// unknown fields included; the protocol.MaxKipuBytes cap and the
	// forward-compatible storage of unknown fields both live in the kipu
	// package, which is the one that knows what it can and cannot write.
	if err := h.deps.Kipu.Append(req.Kipu, now); err != nil {
		h.deps.Logger.Error("sync: recording kipu failed", "error", err)
	}

	acks, err := h.processOutbound(ctx, cfg, req.Outbound, now)
	if err != nil {
		return nil, err
	}

	resp := &protocol.Response{
		ServerTime: now.Unix(),
		Cursor:     req.Cursor,
		Acks:       acks,
		Config: &protocol.DeviceConfig{
			// Content knobs only. No page counts, no chars_per_page, no layout
			// numbers of any kind — reflow is device-owned because font size is
			// a runtime accessibility setting (§4.9, A.10).
			MaxLetterChars: cfg.Sync.MaxLetterChars,
			SyncIntervalS:  cfg.Sync.IntervalS,
			RAT:            cfg.DeviceConfig.RAT,
			Cover:          cfg.DeviceConfig.Cover,
		},
	}

	// Exempt from the byte budget and shipped only on version change (§4.6):
	// at <=24 contacts it is ~1.2 KB worst case, and a half-applied contact
	// list would be worse than a 3 KB response. The store's device view is what
	// keeps addresses off this wire (I-2) while keeping tombstones on it (§7.2).
	resp.Ayllu = h.deps.Ayllu.DeviceView(req.AylluVersion)

	uidValidity, lastUID, err := h.assembleLetters(ctx, cfg, req.Cursor, resp)
	if err != nil {
		return nil, err
	}
	resp.Cursor = encodeCursor(uidValidity, lastUID)

	counter, err := h.commitSync(now, uidValidity, lastUID, req.PututuCounterSeen)
	if err != nil {
		return nil, err
	}
	resp.PututuCounter = counter

	return resp, nil
}

// commitSync writes the per-sync state: the cursor mirror, the sync timestamp,
// and the reconciled doorbell counter. It returns the counter to echo (§10.3).
//
// LastSyncAt is what pututu's skip-if-recently-synced check reads (§10.1). A
// more-drain loop calls this once per round and MUST still count as ONE sync
// for coalescing — ten rounds of one wake are one "the device is awake and has
// our mail" event, not ten (§4.6, §10.1). Wave 3's coalescing logic depends on
// reading this as a timestamp, not a counter, for exactly that reason.
func (h *Handler) commitSync(now time.Time, uidValidity, lastUID uint32, counterSeen uint64) (uint64, error) {
	var counter uint64
	err := h.deps.State.Update(func(s *state.State) error {
		s.LastSyncAt = now
		// Mirror only (§4.4): this value is never read back as the delivery
		// position — the request cursor is.
		s.UIDValidity = uidValidity
		s.LastUID = lastUID
		s.ReconcilePututuCounter(counterSeen)
		counter = s.PututuCounter
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("syncsvc: persisting sync state: %w", err)
	}
	return counter, nil
}

// assembleLetters fills resp.Letters under the byte budget and reports the
// cursor position to mint (§4.4, §4.6).
func (h *Handler) assembleLetters(ctx context.Context, cfg *config.Config, cursor string, resp *protocol.Response) (uidValidity, lastUID uint32, err error) {
	uidValidity, err = h.deps.Mailbox.UIDValidity(ctx)
	if err != nil {
		return 0, 0, unreachableError(fmt.Errorf("syncsvc: uidvalidity: %w", err))
	}

	cursorValidity, cursorUID, ok := decodeCursor(cursor)

	// A cursor whose uidvalidity no longer matches is treated exactly as "" —
	// no special path and no error to the device, so UIDVALIDITY resets are
	// invisible on the wire and need no firmware path (§4.4, test V-21).
	resync := !ok || cursorValidity != uidValidity
	lastUID = cursorUID
	if resync {
		lastUID = 0
	}

	var (
		raws   []mailbox.Raw
		capped bool
	)
	if resync {
		// Window resync: the most recent resync_window letters, not the whole
		// multi-year archive. The deep archive lives in the mailbox and
		// graduates with it (§4.4, test V-2).
		raws, err = h.deps.Mailbox.Recent(ctx, cfg.Sync.ResyncWindow)
		if err != nil {
			return 0, 0, unreachableError(fmt.Errorf("syncsvc: window resync: %w", err))
		}
	} else {
		raws, err = h.deps.Mailbox.FetchAbove(ctx, cursorUID, fetchBatch)
		if err != nil {
			return 0, 0, unreachableError(fmt.Errorf("syncsvc: fetch above uid: %w", err))
		}
		// A full batch means the mailbox may hold more above it; the window
		// path has nothing above its newest message by construction.
		capped = len(raws) == fetchBatch
	}

	budget := h.budgetFor(resp, cfg.Sync.BudgetBytes)
	delivered := 0

	for _, raw := range raws {
		letter, derr := h.deps.Deriver.Derive(ctx, raw)
		if derr != nil {
			var unresolved *derive.UnresolvedSenderError
			if errors.As(derr, &unresolved) {
				// A sender that resolves against nothing, tombstones and past
				// addresses included, is a stranger: reconciliation should
				// already have moved it to Held (§5.1) and this is the same
				// decision arriving late. Skipping it delivers nothing to the
				// device and loses nothing — the message is still in the
				// mailbox for the guardian review flow (§8) — so the cursor
				// advances past it rather than wedging behind it.
				h.deps.Logger.Warn("sync: skipping unresolved sender, expected Held",
					"uid", raw.UID, "letter_id", unresolved.LetterID)
				lastUID = raw.UID
				continue
			}
			// Anything else — a message that will not parse at all — stops
			// assembly rather than being skipped. The cursor is a single
			// watermark, so advancing past a message we could not render would
			// lose it; stopping leaves it blocking, loudly and in the logs,
			// until the cause is fixed or a guardian moves it (§5.2).
			h.deps.Logger.Error("sync: derivation failed, stopping assembly", "uid", raw.UID, "error", derr)
			if delivered == 0 {
				return 0, 0, &statusError{code: http.StatusInternalServerError, err: derr}
			}
			resp.More = true
			return uidValidity, lastUID, nil
		}

		size, merr := letterSize(letter)
		if merr != nil {
			return 0, 0, fmt.Errorf("syncsvc: sizing letter %s: %w", letter.ID, merr)
		}
		// Always include at least one complete letter, whatever it costs: the
		// budget is a cost target, not a transport ceiling, and letters are
		// atomic on the wire (§4.6).
		if delivered > 0 && budget-size < 0 {
			resp.More = true
			return uidValidity, lastUID, nil
		}

		budget -= size
		resp.Letters = append(resp.Letters, letter)
		lastUID = raw.UID
		delivered++
	}

	resp.More = capped
	if delivered > 0 {
		h.deps.Logger.Info("sync: delivered letters",
			"count", delivered, "ids", letterIDs(resp.Letters), "more", resp.More)
	}
	return uidValidity, lastUID, nil
}

// budgetFor returns how many bytes of letters fit in budgetBytes once the rest
// of the response is accounted for (§4.6). The ayllu block is subtracted back
// out because it is explicitly exempt.
func (h *Handler) budgetFor(resp *protocol.Response, budgetBytes int) int {
	skeleton := *resp
	skeleton.Ayllu = nil
	skeleton.Letters = nil
	// A cursor is minted after assembly; charge its (fixed) length now so the
	// budget does not drift by the length of an empty string.
	skeleton.Cursor = encodeCursor(0, 0)

	encoded, err := json.Marshal(&skeleton)
	if err != nil {
		// Response marshalling is exercised on every sync and cannot fail for
		// these field types; treat an impossible failure as a zero-size
		// skeleton rather than failing the sync over accounting.
		h.deps.Logger.Error("sync: sizing response skeleton failed", "error", err)
		return budgetBytes
	}
	return budgetBytes - len(encoded)
}

// letterSize is a letter's cost on the wire: its JSON plus the one byte of
// separator that joins it to the array.
func letterSize(l protocol.Letter) (int, error) {
	encoded, err := json.Marshal(l)
	if err != nil {
		return 0, err
	}
	return len(encoded) + 1, nil
}

// letterIDs extracts ids for logging. Ids only — never subjects, never bodies
// (I-1).
func letterIDs(letters []protocol.Letter) []string {
	out := make([]string, 0, len(letters))
	for _, l := range letters {
		out = append(out, l.ID)
	}
	return out
}
