// Command wirefixtures emits canonical POST /sync payloads from the server's
// own wire types, for the firmware's host tests to parse and re-emit.
//
// Why (chaski-implementation-plan §2): the firmware mirrors internal/protocol
// in C++. Two hand-maintained descriptions of one contract drift, and the drift
// is silent — it shows up as a device that stops understanding its server after
// a server change nobody thought was breaking. Generating fixtures from the Go
// types and asserting the C++ side round-trips them means the mirror can only
// break a test, never a device in a pocket.
//
// Regenerating these is part of any wire change. The server stays the single
// writer of the wire's shape.
//
// Usage: go run ./tools/wirefixtures -out test/firmware/host/testdata/wire
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tholent/chaskiwasi/internal/protocol"
)

type fixture struct {
	name string
	why  string
	v    any
}

func fixtures() []fixture {
	return []fixture{
		{
			name: "request_heartbeat",
			why:  "the normal empty sync: cursor and version only (server §4.2)",
			v: protocol.Request{
				Cursor:       "b64cursorAAAA",
				AylluVersion: 7,
			},
		},
		{
			name: "request_full",
			why:  "every optional field populated, including a full outbox and kipu",
			v: protocol.Request{
				Cursor:            "b64cursorBBBB",
				AylluVersion:      8,
				PututuCounterSeen: 41,
				Kipu: map[string]any{
					"battery_pct": 84,
					"charging":    false,
					"rat":         "ltem",
					"rssi":        -97,
					"queue_depth": 2,
					"fw":          "0.1.0",
				},
				Outbound: []protocol.Outbound{
					{LocalID: "o-000123", ContactID: "c_07", Subject: "camping!", Body: "we went to the lake"},
					{LocalID: "o-000124", ContactID: "c_02", Body: "no subject on this one"},
				},
			},
		},
		{
			name: "request_window_resync",
			why:  "empty cursor means window resync; the device never parses a cursor (server §4.4)",
			v:    protocol.Request{Cursor: "", AylluVersion: 0},
		},
		{
			name: "request_emoji_body",
			why:  "worst-case graphemes on the wire: ZWJ family, flag, skin tone, combining accent",
			v: protocol.Request{
				Cursor:       "b64cursorCCCC",
				AylluVersion: 8,
				Outbound: []protocol.Outbound{{
					LocalID:   "o-000125",
					ContactID: "c_07",
					Subject:   "hola 👋🏽",
					Body:      "familia 👨‍👩‍👧‍👦 desde 🇵🇪 — café ☕ está muy bien",
				}},
			},
		},
		{
			name: "response_letters",
			why:  "inbound letters with each flag combination the device renders differently",
			v: protocol.Response{
				ServerTime:    1785420202,
				Cursor:        "b64cursorDDDD",
				PututuCounter: 41,
				Letters: []protocol.Letter{
					{ID: "l-9f3a2c41d0", ContactID: "c_07", Subject: "camping", Date: 1785349200,
						Body: "We went up to the lake on Saturday.", Trimmed: true},
					{ID: "l-1122334455", ContactID: "c_02", Subject: "long one", Date: 1785349900,
						Body: "This letter was cut at the cap.", Truncated: true},
					{ID: "l-aabbccddee", ContactID: "c_sys", Subject: "Your list changed", Date: 1785350000,
						Body: "Rosa was removed from your list. You can still read her old letters."},
					{ID: "l-5566778899", ContactID: "c_03", Subject: "degraded", Date: 1785350100,
						Body: "Strip was down when this was derived.", Degraded: true},
				},
				More: false,
			},
		},
		{
			name: "response_all_ack_statuses",
			why:  "every terminal ack the device must handle, including forward-compat unknown (D-5)",
			v: protocol.Response{
				ServerTime: 1785420300,
				Cursor:     "b64cursorEEEE",
				Acks: []protocol.Ack{
					{LocalID: "o-000001", Status: protocol.AckSent},
					{LocalID: "o-000002", Status: protocol.AckRejectedInactive},
					{LocalID: "o-000003", Status: protocol.AckRejectedUnknownContact},
					{LocalID: "o-000004", Status: protocol.AckInvalid},
					{LocalID: "o-000005", Status: protocol.AckRejectedUndeliverable},
					{LocalID: "o-000006", Status: protocol.AckStatus("some_future_status")},
				},
			},
		},
		{
			name: "response_ayllu_and_config",
			why:  "contact block with a tombstone and c_sys, plus pushed config (server §7.2, A.15)",
			v: protocol.Response{
				ServerTime: 1785420400,
				Cursor:     "b64cursorFFFF",
				Ayllu: &protocol.Ayllu{
					Version: 8,
					Contacts: []protocol.AylluContact{
						{ID: "c_02", Name: "Abuela", Active: true, Pinned: true, Order: 0, Portrait: "p01"},
						{ID: "c_07", Name: "Rosa", Active: false, Order: 3, Portrait: "p04"},
						{ID: "c_sys", Name: "Wasi", Active: false, Order: 99},
					},
				},
				Config: &protocol.DeviceConfig{
					MaxLetterChars: 500,
					SyncIntervalS:  21600,
					RAT:            "ltem",
					Cover:          "road",
				},
			},
		},
		{
			name: "response_more_true",
			why:  "the drain loop: device syncs again immediately, capped at 10 rounds (server §4.6)",
			v: protocol.Response{
				ServerTime: 1785420500,
				Cursor:     "b64cursorGGGG",
				Letters: []protocol.Letter{
					{ID: "l-0011223344", ContactID: "c_02", Subject: "one of many", Date: 1785350200, Body: "first"},
				},
				More: true,
			},
		},
	}
}

func main() {
	out := flag.String("out", "test/firmware/host/testdata/wire", "output directory")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}

	index := map[string]string{}
	for _, f := range fixtures() {
		b, err := json.MarshalIndent(f.v, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, f.name, "marshal:", err)
			os.Exit(1)
		}
		b = append(b, '\n')
		p := filepath.Join(*out, f.name+".json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		index[f.name+".json"] = f.why
	}

	b, _ := json.MarshalIndent(map[string]any{
		"note": "Generated by tools/wirefixtures from internal/protocol. Do not hand-edit. " +
			"Regenerate on any wire change; the firmware's C++ mirror is asserted against these.",
		"fixtures": index,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "index.json"), append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write index:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d wire fixtures to %s\n", len(index), *out)
}
