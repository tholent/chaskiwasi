# Chaskiwasi — Design Specification

*Draft v0.3 — July 2026*

A small, text-only communications device that lets a young person who moves
frequently stay in contact with extended family, without a phone, a phone
number, a monthly bill anyone has to remember, or a social feed.

---

## 0. Naming

| | | |
|---|---|---|
| **Chaskiwasi** | the project | the relay hut on the road |
| **Chaski** | the device | the runner |
| **Wasi** | the server | the house |

Everything else stays in plain English.

A *chaski* was a relay runner on the Inca road system. He never ran the whole
route — he ran his leg of the Qhapaq Ñan and handed off at a *chaskiwasi*, a
hut where the next runner waited. That is store-and-forward, in 1450. The
runner blew a conch as he approached so the next one was already standing: the
notification doorbell, also in 1450.

The compound is the system; its two halves are the two components. Nothing
needed to be invented to make that line up.

Pronunciation: CHAS-kee, WAH-see, CHAS-kee-WAH-see. All survive cold reading —
no ambiguous digraphs, no apostrophes, no ñ. The project name is four syllables,
but it lives in repo names and this document. The kid only ever says *Chaski*.

**Three words is the ceiling — for user-facing surfaces.** The test that sets
it: **if someone who has lived in the Andes can't pronounce it confidently on
sight, it's out.** Quechua `ll` is a palatal lateral, but Spanish yeísmo has
collapsed that distinction across much of Latin America, so *ayllu* has no
correct answer to look up. That is the worst kind of friction on a screen a
child reads. Apply the same test to anything user-facing added later.

Guardian-facing surfaces stay in English regardless — "Contacts," "Held
Messages" — because a great-aunt on an old laptop needs clarity, not immersion.

### 0.1 Internal vocabulary

Code has different constraints. Identifiers are read, not spoken, so the
pronunciation test does not apply — and the richer glossary comes back:

| Identifier | Meaning | Refers to |
|---|---|---|
| `pututu` | the conch blown on approach | the SMS notification hook |
| `ayllu` | the extended kinship group | the contact list |
| `kipu` | the knotted-cord administrative record | the telemetry package |

These earn their place on a practical argument, not a decorative one:
**they are unambiguously greppable.** `notify` or `push` returns hundreds of
false positives across a codebase; `pututu` returns exactly the notification
path. `contacts` appears everywhere; `ayllu` appears only where the allowlist
is meant. A distinctive identifier is a search index.

Suggested usage — the server endpoint Hologram calls, the device-side wake
handler, the SQLite table, and the contacts version field in the sync
response:

```
POST /pututu          →  outbound SMS trigger (Wasi)
pututu_handle_wake()  →  device-side wake path (Chaski)
ayllu                 →  SQLite table of resolved contacts
ayllu_version         →  contacts revision in the sync response
```

**Hard boundary: these names never cross into UI strings.** Not in error
messages, not in log lines a guardian might see, not in an API field that gets
rendered. Keep all user-facing text in a single strings file so the boundary is
structural rather than a matter of discipline.

Put a two-line glossary comment at the top of each module that uses them. A
future contributor — or a future you — should not have to guess.

**On name collisions:** there is an Android chat app called Chasky and several
companies called Chaski; *pututu* is likewise taken, including by a push
notification service that reached for the same conch metaphor independently.
None of this matters here. **The device has no marketing surface** — no app
store listing, no website the kid visits, no search anyone performs. The name
appears on the boot screen, on the case, and in conversation with family.
Trademark protects against confusion within a class of goods in commerce; a
messaging app, a logistics company, and a device for one family are not in
conflict, and the existing coexistence shows nobody holds a strong claim on a
word this descriptive. Internal identifiers are invisible to all of it.

If this ever becomes something dozens of families buy, rename then — trivial at
one user, impossible at fifty thousand — and run a USPTO Class 9 search at the
same time. Do **not** bolt letters on for distance (Chaskii, Chaskee, Chask);
deliberate misspelling reads as a lost domain fight and breaks the on-sight
pronunciation the name was chosen for.

Let "Chas" emerge on its own if it's going to. Nicknames you design read as
marketing; nicknames that emerge mean the kid has adopted the thing.

---

## 1. Design Principles

These are the commitments that everything else follows from. Where a later
decision conflicts with one of these, the principle wins.

1. **Text only.** No camera, no browser, no feed, no apps, no notifications
   from anyone but family.
2. **Non-realtime.** Letters, not chat. This is what makes weeks of battery
   life and intermittent connectivity acceptable rather than broken.
3. **The device never handles email addresses.** It knows contact IDs and
   display names. Address resolution happens server-side, always.
4. **Store-and-forward everywhere.** The kid can type any time. Delivery
   happens when it happens. The device never appears broken for lack of signal.
5. **The mailbox is canonical.** The device gets a derived view. Nothing is
   rewritten or deleted server-side. This archive should survive to adulthood.
6. **It must not stop working because a bill lapsed.** Connectivity is
   prepaid in bulk, not subscribed monthly.
7. **Graduation is the end state.** When the youth ages out, the constraints
   fall away and what remains is a normal email account with full history.
8. **The screen reveals nothing at rest.** E-ink holds its last image with zero
   power — including after the battery dies. A device left on a table, dropped
   in a bag, or found flat must not be showing someone's letter.

---

## 2. System Architecture

```
  [ CHASKI ]  --LTE-M-->  [ WASI ]  --IMAP/SMTP-->  [ FASTMAIL ]  <-->  family
   ESP32-S3              sync endpoint,             canonical mailbox,
   + GM02SP              contact resolution,        guardian access
   the runner            quote stripping            the record
                         the hut on the road
```

**Chaski → Wasi:** a single HTTPS `POST /sync`. Device sends its cursor and any
queued outbound letters; server returns new letters, acks, and a contacts
version. TLS terminated on the modem's built-in stack.

**Wasi → Fastmail:** standard IMAP/SMTP. Wasi holds IDLE where power is free.

**Fastmail → family:** ordinary email. No app for relatives to install, no
account to create, no platform to choose.

### Why email and not a chat protocol

Store-and-forward is email's native design, not a workaround bolted onto it.
Mail queues server-side indefinitely, there is no session or presence to
maintain, and the device can be dark for a week without losing anything. Every
relative already has an address. And an email address is portable identity —
for someone whose life keeps getting packed into boxes, that continuity has
value independent of the hardware.

---

## 3. Server Design

### 3.1 Contact resolution

The device composes `{to: "c_07", body: "..."}`. The server resolves `c_07` to
an address and sends. Inbound, the server resolves the From address back to a
contact ID and **drops anything that does not resolve**.

The resolution table is `ayllu` (§0.1). It is the only place addresses exist.

Consequences, all of which come free from this one decision:

- The send allowlist is enforced **by construction**. There is no code path
  that could address a letter to someone not on the list, even with modified
  firmware.
- Inbound default-deny is the same mechanism running backwards.
- A lost or stolen device leaks **no addresses at all** — only first names.

### 3.2 Address book

- Canonical list is **guardian-managed** via a small web UI on Wasi.
- The device syncs it; the youth cannot add or remove contacts.
- The youth **does** own everything cosmetic: nicknames, ordering, pinning
  favourites, 1-bit portraits. That is most of what makes an address book feel
  like yours.
- *Deferred:* a request flow where the youth proposes a contact who appears
  greyed out and unsendable until a guardian approves. One table, one button.
  Not needed for v1.

### 3.3 Inbound handling

- **Quarantine, do not bounce.** Non-allowlisted mail files into a `Held`
  folder guardians can review. A bounce confirms the address exists to whoever
  probed it; silent deletion means nobody discovers that a new cousin was never
  added.
- Use an unguessable local part. Never print the address on the device.
- *Optional:* per-contact inbound aliases, so a leaked address can be revoked
  individually instead of migrating the whole mailbox.
- Render `text/plain` only. Never load remote images (kills tracking pixels for
  free). Ignore attachments in v1.

### 3.4 Reply and signature stripping

Heuristic, with no correct general solution. Ordered most to least reliable:

1. Take the `text/plain` part; never parse HTML when plain exists.
2. `-- ` (dash-dash-space) alone on a line — a real standard delimiter.
3. Lines prefixed `>`; strip trailing blocks.
4. Attribution lines (`On [date], [name] wrote:`) — variable, localised,
   often wrapped. Best effort.
5. Outlook's `-----Original Message-----`, underscore rules, and
   `From:/Sent:/To:` header blocks.
6. Handle `format=flowed` soft breaks correctly, or text rewraps badly on a
   narrow screen.

Use `email-reply-parser` or Mailgun's `talon`; do not write this from scratch.
Log what gets stripped so it can be tuned. Show a small indicator on-device
when content was trimmed. Because guardians hold the real mailbox, a bad strip
is recoverable rather than lost.

### 3.5 Threading and headers

Generate a proper `Message-ID` for every outbound letter, and set
`In-Reply-To` and `References` on replies. The device UI never shows threads,
so this is easy to skip — but skipping it leaves the graduated account and
every relative's mail client with years of orphaned messages. Trivial now,
unfixable later.

Server also generates subject lines (first few words, or "Letter from Rosa"),
so the graduated account isn't years of blank subjects.

### 3.6 Storage

SQLite. Two tables plus a device cursor. All transforms — quote stripping,
contact resolution, pagination — happen at **read time** against the canonical
mailbox.

### 3.7 Telemetry — the `kipu`

A small status record Chaski attaches to each sync. The name fits: a khipu was
an *administrative* record — census, tribute, inventory — which is what this is.

**It never initiates a connection.** The kipu rides along on the existing sync
POST. A packet that woke the radio on its own schedule would cost more than
everything it reports.

**It carries no precise position, ever.** Chosen location is content and lives
in letters (§4.2). The kipu is observed data only.

#### Tiers

| Tier | Contents | Default | Cost |
|---|---|---|---|
| **1 — Health** | battery %, charging state, RAT, signal, last sync, queue depth, firmware version | on, not disableable | free |
| **2 — Coarse position** | serving cell (MCC/MNC/LAC/CID) | **on, opt-out** | free |

**Say the quiet part plainly: tier 2 is location.** A serving cell ID resolves
against public tower databases to a few hundred metres in a city. Calling it
"network diagnostics" would be self-deception, and the spec should not pretend
otherwise.

Two things follow from being honest about that. It gets the same agency
treatment as anything else location-shaped — the child can turn it off. And it
is worth noting what turning it off does *not* do: the carrier knows the
serving cell regardless. Opting out stops Wasi from recording it. It does not
make the device untracked, and the child should not be told otherwise.

**Opting out of tier 2 does not disable "where I am."** A child who has turned
off background reporting can still deliberately attach their position to a
letter, including the tower-derived fallback (§4.2). Chosen and observed are
separate all the way down.

#### Transparency mechanisms

Disclosure is a *mechanism*, not a promise in a document.

1. **Changes arrive as letters, in both directions.** Tier 2 being turned off
   or back on generates a letter to *both* the child and the guardian, in the
   same inbox as letters from family. Nothing changes silently, and neither
   party can change it behind the other's back.
2. **The notification is the safety mechanism.** This is the reason opt-out is
   acceptable rather than a hole in the design. If someone pressures a child
   into going dark, the disabling itself sends a letter — so the outcome is not
   silence, it is a signal. A guardian seeing *"position reporting off since
   Tuesday"* has exactly the prompt they need, which was the protective
   function they actually wanted.
3. **A readable log of what was sent.** Plain language, on-device, any time:
   *"Tue 15:04 — battery 64%, LTE-M, near tower 310-260-1234."* Not a hex dump.
   The child can always answer "what does it know about me" without asking
   anyone.

#### Storage and retention

**The kipu never enters the mailbox.** It lands in a separate table on Wasi,
behind the guardian admin login.

This follows directly from Principle 5. The mailbox is canonical and survives
to adulthood — and nobody should be handed a decade of their childhood location
history at eighteen, in an account whose archive they are told never to alter.
Telemetry is explicitly excluded from the graduation handover (§9).

- Tier 1 health: 90 days rolling.
- Tier 2 position: **30 days rolling, hard auto-purge.**

Short retention is a safety feature, not housekeeping. In a complicated
household — separated parents, contested custody, a relative who knows the
password — an accumulated location history is a weapon. A store that cannot
accumulate one cannot have it taken from it.

---

## 4. Device Experience

The framing is **game mail, not email.** The useful thing this buys is not
aesthetic: game mail has always had delivery delays and players have never
minded. Store-and-forward latency is a technical constraint, but a runner on a
road is a *story*.

> "On the road" reads as how the world works.
> "Queued — no connection" reads as broken.
>
> Same state. Completely different experience for a kid.

**Vocabulary:** letters, not messages. **Waiting → on the road → arrived.**
The road carries the whole story, and nobody needs to know what a wasi is to
read it.

**Composition:** pick a person by face and name, *then* write. Never a To:
field.

**Outbox:** letters visibly sit there until the runner takes them.

**Sync button:** a physical mail flag. Dedicated switch, not a keypad key — it
should work even if the UI is confused. The **put-away key** (§4.1) is the
second such switch, for the same reason.

**Length:** 500 characters outbound. A feature, not a restriction — it makes
writing feel low-stakes and keeps rendering simple. Inbound letters that exceed
it are **paginated, never truncated.** Never drop words from family.

### 4.1 Screen wipe on sleep

E-ink's bistability is a privacy hazard, not just a power win. The last image
persists indefinitely with no power at all — including after a flat battery, in
a bag, on a table, in someone else's hands. Nothing about the hardware clears
it. **The wipe must be proactive, on every sleep, not a shutdown step.** A
device that dies unexpectedly cannot run a shutdown step.

**Three triggers:**

| Trigger | Behaviour |
|---|---|
| Inactivity timeout (~45s) | wipe, then deep sleep |
| **Put-away key** | immediate wipe, no confirmation, no delay |
| Battery below ~5% | wipe to a charge-me cover, refuse to open content |

The put-away key matters more than the timeout. The worry here isn't really
about a screen left on too long — it's about **agency**. A kid needs a single
deliberate gesture that hides everything *now*, without navigating anywhere.
From any screen, no confirmation dialog.

**Assign it to a KeebDeck top-row key**, not a discrete switch. A separate
switch buys nothing real here: either way firmware has to run the clear
sequence, so there is no true hardware-only path to protect. What matters is
firmware layering, not physical separation.

Two requirements follow:

- **Handle it in the input layer, below UI dispatch.** The reason it wanted to
  be a discrete switch was so a hung screen couldn't swallow it. Get that from
  priority instead: the put-away scancode is intercepted before any screen's
  handler sees it, and a watchdog covers the rest.
- **Make it findable by touch.** In a row of 69 identical silicone keys, a
  panic gesture needs to be locatable without looking. Use the **corner-most**
  top-row key and give it a distinct cap — colour, or a raised nub. KeebDeck
  offers custom key labels at 500+ units.

The **sync key** can live on the top row on the same terms.

**The cover screen** is what's left behind. Never a blank white panel — that
reads as broken or dead. Show the Chaski wordmark or a road motif, plus battery
state. It must reveal nothing about content, sender, or activity.

*Open:* whether to show a waiting-letter count on the cover. Useful, but even
"3 waiting" is a conversation someone else can start. Sender names on the cover
are out regardless.

**Clearing sequence — the part that's easy to get wrong.** A single full
refresh is **not** sufficient. After a long composing session of partial
refreshes, residue remains and faint text stays legible under angled light. Use
the driver's full clear waveform — an alternating black/white flush, several
passes — not one full update. GxEPD2's `clearScreen()` does this.

Then sequence into sleep in this order, or risk freezing the panel mid-wipe,
which is worse than not wiping at all:

```
1. clear waveform (multi-pass flush)
2. wait for BUSY to deassert
3. render cover screen
4. wait for BUSY to deassert
5. SSD1680 deep-sleep command
6. cut the peripheral rail (§5.5)
7. ESP32-S3 deep sleep
```

**Cost:** negligible in energy — a full refresh is 12mW for 2s, roughly 1.8µAh;
even a hundred wipes a day is under 0.2 mAh. The real cost is ~3–4 seconds of
flashing.

**Don't hide that flashing.** It's the confirmation. A kid pressing put-away
and watching the panel flash black-white-black *sees* the letter destroyed.
That visible evidence is worth more than the seconds it costs, and hiding it
behind a splash would waste the best reassurance the hardware can offer.

*Open (§11):* whether waking from the cover requires a PIN. It protects against
someone picking the device up, at the cost of friction and a forgettable
secret. Not a given either way.

### 4.2 "Where I am" — location as content

There is **no continuous precise-location mode.** GNSS is used in exactly one
place: the child can attach their position to a letter they are already
writing, to a person they have already chosen.

**The line this draws:**

> **Chosen location is content. Observed location is telemetry.**
>
> Content goes in a letter, to one named person, and lives in the archive like
> any other sentence. Telemetry is observed, never precise, and purged in 30
> days (§3.7).

**Why this is better than a background mode:**

- It inherits every existing guarantee for free — the allowlist, contact
  resolution, the mailbox, retention. No new policy surface at all.
- It deletes the entire "guardian enables tracking" flow: no config push, no
  cover-screen tracking glyph, no argument about whether a guardian can force
  it on.
- **The power problem vanishes.** A cold GNSS fix costs roughly 0.3 mAh; hourly
  background fixes would have added 5–7 mAh/day to a ~16 mAh/day budget, and
  indoors would have cost full price and returned nothing. One fix, only when
  asked for, is free by comparison.
- It is a much simpler promise to make to a child: *nothing on this device
  reports where you are unless you put it in a letter.*

**What it costs, and this is real:** if the child is in trouble and does not or
cannot send it, there is no precise position. A guardian wanting find-my-kid
does not get it. Tier 2's serving cell remains as a coarse breadcrumb, but that
is a few hundred metres in a city and much worse in the country. Choose this
knowingly.

**Flow:**

1. Child presses **"add where I am"** while composing.
2. The GNSS fix starts immediately; they keep typing. A cold fix can take
   30–60s.
3. Chaski also captures the **current serving cell** at the same moment,
   regardless of the GNSS outcome and regardless of the tier 2 setting.
4. Whichever resolves, the letter shows plainly what will be attached before
   sending.

#### Degradation ladder

| Rank | Source | Typical accuracy | Presented as |
|---|---|---|---|
| 1 | GNSS fix | 5–20 m | place name + map pin + link |
| 2 | Serving cell + neighbours | 200 m – 1 km | place name + stated radius |
| 3 | Serving cell alone | 1–10 km urban, far worse rural | town/area name + stated radius |
| 4 | Nothing available | — | *"Couldn't find where you are."* |

If the modem can report **neighbouring cells** as well as the serving cell,
send them. Trilateration across several towers is substantially better than a
single-tower centroid, and it costs nothing extra.

**Degrade the representation, not just the data.** This is the part that is
easy to get wrong and dangerous to get wrong. A tower-derived position rendered
as a map pin looks exactly as authoritative as a GNSS fix, and a grandmother
will drive to it. Rank 1 gets a pin. Ranks 2–3 get a **place name and an
explicit radius**, never a pin, never bare coordinates. Reverse-geocode to
something human — *"near Ferndale, MI (approximate, within about 3 km)"*.

Be honest that rural accuracy is poor. A single rural tower can cover tens of
kilometres, and the honest rendering there is a county, not a location.

#### Timestamp it — non-negotiable

**Store-and-forward makes an untimed position actively misleading.** A letter
written at 15:04 may not sync until 19:30. "Where I am" must mean where the
child was *when they wrote it*, so:

- **Capture at compose time, never at send time.** A position captured when the
  radio happens to come up describes somewhere the child may have left hours
  earlier.
- **State the capture time in the letter**, in the recipient's terms:
  *"Rosa was near Ferndale at 3:04pm — sent 7:30pm."*

A position without a capture time, in a system with delayed delivery, is worse
than no position at all.

#### Resolution happens on Wasi

Chaski sends the raw cell identifiers. It carries no tower database and makes
no geolocation call. Wasi resolves identifiers to coordinates and a place name
when rendering the outgoing mail.

**Resolve against a local snapshot, never a third-party API.** Querying Google
or Unwired Labs would hand a named minor's location to an outside party on
every request — which would undo most of what the rest of this document is
for. Mozilla Location Service is closed; **OpenCelliD** publishes a downloadable
dataset under CC-BY-SA. Import the relevant country subset into SQLite on Wasi,
index on `(mcc, mnc, lac, cellid)`, and refresh it occasionally. Lookups become
a local query with no external calls at all.

**Format in the outgoing mail:** a human-readable line plus a maps link for
rank 1, so a grandmother taps once rather than copying coordinates. Ranks 2–3
get the place name and radius, and a link only to a map *area* if at all.

**Note the archive consequence:** unlike the kipu, an attached position *does*
enter the mailbox and *does* graduate. That is correct — the child chose to
send it, to that person, as part of what they said.

---

## 5. Hardware

### 5.1 Core module — Walter (DPTechnics)

ESP32-S3-WROOM-1-N16R2 (16MB flash, 2MB PSRAM) + Sequans Monarch 2 GM02SP.
€49.95 bare / €69.95 with certified antennas. **Skip the €250 devkit** — it
bundles a Soracom SIM that isn't needed.

Chosen for three reasons beyond the specs:

- **10-year availability guarantee.** For a device whose point is lasting until
  a kid graduates, this is the most valuable line in the datasheet.
- **Certified for FCC, CE, UKCA, IC, RCM.** Unless another radio is added,
  certification cost and risk stay minimal. This largely erases the regulatory
  burden.
- **Integrated, certified RF.** No antenna design, no board spin to fix it.

Provides LTE-M, NB-IoT (Cat-NB1/NB2), WiFi b/g/n, BLE 5.0, GNSS.

### 5.2 Keyboard — Solder Party KeebDeck

The BlackBerry BBQ20 is discontinued with no known restock; Solder Party hit
the same sourcing wall and designed KeebDeck to replace it. Harvested BB parts
are a dead end for any production run.

- 85 × 48mm, 69-key silicone keypad, orthogonal layout
- Ships as keypad + adhesive metal dome sheet — no PCB, no MCU, no interface
- Sandwich: PCB footprint → dome sheet → keypad → top case, 2.0mm spacing
- Optionally backlit; custom key labels available at 500+ units
- **KeebDeck Basic** (STM32F042, QMK, or alt firmware with I2C) for prototyping

Advantages over BBQ20 beyond availability:

- **Kills the keyboard MCU.** Passive matrix scanned directly from ESP32-S3
  GPIO, with `ext1` deep-sleep wake on any keypress. No power gating, no I2C
  wake dance, one less part — and it removes what was a candidate for the
  largest static draw in the design.
- Sets device width at ~90mm, which stacks cleanly with the display.

**Cost:** no trackpad. The UI is a contact list and a text field, so arrow keys
suffice — but design for it deliberately.

**Sourcing:** buy keypads for the entire production run up front. This is the
part with real supply risk and it is cheap to over-buy.

### 5.3 Display — Good Display GDEY027T91-FL02

2.7" frontlit e-paper, bonded assembly (frontlight is not sold separately).

| | |
|---|---|
| Resolution | 264 × 176 |
| Controller | SSD1680Z8 |
| Outline / active | 70.42 × 45.8 × 1.8mm / 57.29 × 38.19mm |
| Refresh | partial 0.3s, fast 1.5s, full 2s |
| Greyscale | 4 |
| Interface | SPI, 24-pin 0.5mm FPC |
| Standby | 0.003 mW |
| Operating temp | 0–50 °C |
| Frontlight | 6-pin FPC, 4 LEDs series, ≤12V, ≤15mA |

Partial refresh is confirmed and non-negotiable — full-refresh-only panels are
unusable for typing. SSD1680 is very well supported (GxEPD2), so display
bring-up is close to free.

**Sizing note:** at a 6×13 font this holds ~570 characters, so **one
500-character letter is exactly one screen** — no scrolling to read or compose.
It also keeps the device genuinely pocketable, which matters for someone who
moves around.

**Open:** the 4.2" GDEY042T81-FL02 (400 × 300, ~91mm wide) matches KeebDeck
width better and shows ~1500 characters, at the cost of a small-ereader-sized
device. Decide by holding both.

### 5.4 Frontlight driver

4 LEDs in series at ≤12V requires a boost from a single LiPo cell. 12V × 15mA
= 180mW ≈ **57mA off the battery** at full brightness (not the 15mA the panel
spec implies).

Spec a constant-current LED boost driver (TPS61165 or similar) with PWM dimming
and true sub-µA shutdown, since it is off nearly always. Warm/amber LEDs, three
brightness steps, **default off**, dedicated physical button.

No ambient light sensor — it adds quiescent draw and a "why did it do that"
failure mode. A button is more legible to a kid than automatic behaviour.

### 5.5 Power

- Single-cell LiPo, 2000–3000mAh. At the measured idle figures a 2000mAh cell
  still gives ~3.5 months and is meaningfully thinner — probably the right
  trade for a device chosen for pocketability.
- Charger IC and fuel gauge **selection is now a first-order decision** (see
  §7). Verify battery-side quiescent, not just charge performance.
- 1000µF bulk capacitance at the modem rail. TX bursts browning out the rail is
  the classic first-bring-up failure.
- USB-C requires 5.1kΩ CC pulldowns or most chargers deliver nothing.
- Use Walter's MOSFET-switched peripheral rail for the display and frontlight
  boost. The panel is bistable — run the clear sequence and issue the SSD1680
  deep-sleep command **first**, then cut the rail (full ordering in §4.1).

### 5.6 Other

- **No separate RTC chip.** Add a 32.768kHz crystal (~20ppm, costs cents) and
  pull network time from the modem on each sync. A DS3231 is redundant.
- 16MB onboard flash with LittleFS holds tens of thousands of letters. No
  microSD — power draw and a failure point.
- ESP32-S3 hardware flash encryption + secure boot via eFuse. Encryption at
  rest nearly free, and this device will get lost.
- **SIM: nano-SIM socket, inside a screwed-shut case.** MFF2 was the original
  choice, but a soldered SIM makes a spare useless. A socket the kid can't
  reach gets both robustness and recoverability.
- Physically unremarkable enclosure. An old iPhone gets stolen; a weird e-ink
  brick doesn't.

---

## 6. Connectivity

### 6.1 Carrier — Hologram

Pay-as-you-go: $1/month + $0.03/MB, **unlimited to-device SMS** via documented
REST API. This is the `pututu` path (§0.1) — unlimited SMS is what lets Wasi
ring on *every* letter instead of rationing notifications.

Data is a rounding error: with TLS session resumption a sync is ~2KB; ten a day
is ~600KB/month, about **two cents**. Effective bill ≈ $12/year.

**Preload the account balance** with five or ten years' worth. This is what
satisfies Principle 6 — it stops the device depending on anyone's card staying
valid.

### 6.2 Radio access technology — LTE-M primary

NB-IoT has ~8dB better link budget (MCL ~164 vs ~156 dB), roughly one more wall
or floor. It is still the wrong default here:

- **No connected-mode handover.** Idle-mode cell reselection only. A kid on a
  bus re-attaches constantly, and each re-attach costs energy and delay. On a
  moving device the power advantage can invert.
- **Fewer US carriers.** AT&T has decommissioned NB-IoT and moved customers to
  LTE-M; T-Mobile and Verizon retain it. So NB-IoT means two carriers, LTE-M
  means three — fewer roaming partners for a multi-carrier MVNO serving a
  device that moves unpredictably. That is backwards from the goal.
- **SMS is less consistently deployed on NB-IoT**, and the entire notification
  design depends on to-device SMS.

**Make RAT a server-pushed setting, not a compile-time constant.** If a
particular unit ends up somewhere with genuinely bad LTE-M coverage, flip it
remotely instead of shipping firmware.

And note: **store-and-forward already spends that 8dB.** The difference is
"delivers from the basement" versus "delivers on the walk home."

### 6.3 No WiFi, no BLE

Walter carries both radios. **Neither is compiled into the firmware.** Not
disabled at runtime — absent, so there is no code path to reach them.

Opportunistic WiFi was dropped first, for reasons beyond simplicity:

- **It would be the single largest line in the power budget.** A scan is ~120mA
  for a couple of seconds; four an hour is ~6.4mAh/day — roughly 40% of total
  consumption and about six times the entire LTE sync cost.
- Captive portals are effectively unsolvable and were the ugliest unresolved
  item in the design.
- Removes WPA credential entry on a 264×176 screen with no trackpad, network
  lists, signal indicators, and an entire dual-path state machine.

The maintenance-mode exception that once justified keeping a WiFi stack is
gone too: firmware now arrives over USB-C (§6.5). That removes the last WPA
credential entry screen, the hidden menu, and every line of dual-path
connection logic.

**BLE goes with it.** Another radio, another attack surface, no use case.

*Certification is unaffected* — Walter is certified with these radios present;
not using them changes nothing.

**Cost accepted:** cellular is a single point of failure. Mitigate with
preloaded balance, billing alerts, and a spare provisioned SIM in a drawer.

### 6.4 Sleep and wake architecture

Walter idles at **9.8µA** with the ESP32-S3 in deep sleep and the GM02SP in
PSM. Target configuration: **PSM with a 30–60 minute TAU.** The network buffers
the SMS and delivers it at the next wake, so letters land within the hour at
single-digit µA idle.

The key asymmetry: **the ESP32-S3 reboots through `setup()` on every wake; the
modem does not.** The GM02SP is separately powered and holds its network
registration, PDP context, and TLS session across ESP32 sleep cycles.

> **Design rule: expensive state lives in the modem, cheap state gets rebuilt.**
> Network attach is costly and survives. ESP32 heap is free and doesn't matter.

This also dissolves the SMS-wake problem. Only the modem's UART0 connects to
the ESP32-S3 (115200 8N1, hardware handshaking, AT commands), and UART cannot
wake the S3 from deep sleep — but it doesn't need to. **The modem buffers the
SMS.** The ESP32 wakes on the RTC timer every 10–15 minutes and asks the modem
over UART whether anything arrived. That is a local serial transaction costing a
few hundred µAh/day, not a radio event. No interrupt line required.

Check the datasheet's pin and test-pad tables for a routed modem RING line
anyway, but treat it as an optimisation, not a dependency.

**Firmware notes:**

- 8KB RTC slow memory survives deep sleep. Put the sync cursor, unread flag,
  wake reason, and boot counter there. Larger state → NVS, but flash writes
  cost energy and wear, so keep them rare.
- Set `CONFIG_BOOTLOADER_SKIP_VALIDATE_IN_DEEP_SLEEP`. Image verification on
  every wake is pure waste when rebooting dozens of times a day.
- Disable ROM bootloader logging.

### 6.5 Firmware updates — physical only

**Updates require the guardian to have the device in hand, over USB-C.** There
is no over-the-air firmware path at all.

The ESP32-S3 has a native USB Serial/JTAG peripheral, so USB-C flashing needs
no extra chip. The port is already exposed for charging.

**Make it usable by a non-technical guardian.** An `esptool` command line is
hostile to a great-aunt. Use **ESP Web Tools** — the guardian opens a page in
Chrome or Edge, clicks Install, and Web Serial does the rest. No SDK, no
toolchain, no terminal. Host it on Wasi; the kid never sees the URL.

**Signing still applies.** With secure boot and flash encryption enabled via
eFuse (§5.6), only images signed with the project key will boot. Physical
access alone does not let someone flash arbitrary firmware.

**Do not confuse firmware with configuration.** Configuration still moves
freely over LTE via the sync endpoint — contact list, radio access technology
(§6.2), timeout values, cover-screen options. Only executable code requires
physical possession.

**What this buys:**

- **The remote code-execution surface is gone.** A device that cannot receive
  firmware over any network cannot be remotely compromised through one. For a
  device carried by a vulnerable young person, that is worth more than the
  convenience it costs.
- No OTA state machine, no rollback partitions to manage, no interrupted-update
  brick risk, no A/B partition scheme eating flash.

**What it costs, honestly:** a critical bug cannot be fixed remotely. If the
kid is three states away, the fix waits until someone is physically present.
Mitigate by keeping the remotely-adjustable configuration surface wide enough
that most field problems are config problems — and by treating firmware
releases as rare and well-tested, which the absence of an easy update path
enforces anyway.

---

## 7. Power Budget

Estimated daily consumption, 2.7" panel, moderate use:

| Item | mAh/day |
|---|---|
| Walter idle (9.8µA × 24h) | 0.24 |
| EPD standby (0.003 mW) | 0.02 |
| Fuel gauge, hibernating | 0.07 |
| Charger IC quiescent | ~0.36 |
| Hourly PSM TAU wakeups | ~1.0 |
| 6 sync sessions | ~1.7 |
| ~500 partial refreshes | 0.14 |
| **15 min/day active composing** | **~10** |
| LiPo self-discharge | ~2.5 |
| **Total** | **~16** |

At 3000mAh with 85% usable: **roughly five months.** Halving it for pessimism
and carrier-mangled PSM timers still gives two to three months.

**Two conclusions that change where design attention goes:**

1. **The charger IC now draws more than the entire radio and MCU combined.**
   At 9.8µA idle, the boring support parts dominate. Check BQ24075's
   battery-side quiescent against alternatives (BQ25185 is worth a look), and
   confirm the MAX17048 actually enters hibernate rather than sitting at ~23µA.

2. **Active composing swamps everything else by an order of magnitude.** Sleep
   current is essentially solved. The optimisation that matters is how long the
   S3 stays awake while a kid types — batch partial refreshes at word
   boundaries or on a ~200ms idle timer rather than per keystroke, and
   light-sleep between them. Worth more than any static-draw tuning.

*Note: deep sleep current roughly doubles per 10 °C above 25 °C. In a pocket at
35–40 °C expect 15–20µA. Irrelevant at this scale, but worth knowing.*

---

## 8. Privacy and Safeguarding

These are policy decisions that must be **made deliberately and disclosed on
the device.** Drifting into them is the failure mode.

**Guardian mailbox access.** Guardians hold the canonical Fastmail account, so
they can read everything. This is a defensible policy — but the device must say
plainly that it is in effect. A kid who discovers later that it was logging
silently loses trust in the whole thing.

**Location.** Walter includes a GNSS receiver, and the modem always knows its
serving cell. On a device carried by a young person whose guardians already
read the mailbox, this is a tracker waiting to happen. The design answer is to
never build the tracker:

- **No continuous precise-location mode exists.** GNSS is reachable only by the
  child attaching their position to a letter they are writing (§4.2).
- **Coarse serving-cell position is opt-out**, and turning it off notifies both
  parties (§3.7).

**Why opt-out is safe here.** The objection to a disableable breadcrumb is
coercion — someone pressuring a child into going dark. The symmetric-letter
rule answers it: disabling *is itself a notification*. The outcome is a signal,
not silence, and a guardian seeing "position reporting off since Tuesday" has
the prompt they needed.

**What was given up, deliberately:** find-my-kid. If a child is in trouble and
does not send their position, there is no precise fix — only a coarse tower.
That is a real loss and it was chosen, not overlooked. An always-on tracker is
a different product; building this one instead is the decision.

**Telemetry.** Never in the mailbox, 30-day purge on position data, excluded
from graduation. See §3.7.

**In transit.** TLS on the modem stack. E2EE via Autocrypt/Delta Chat was
considered and rejected — it only works for relatives willing to install
software, which erodes the main advantage of using email at all.

---

## 9. Graduation Path

The end state is a plain email account. This imposes one rule on everything
upstream: **the mailbox is canonical and the device gets a derived view.**
Nothing is rewritten in place, nothing is deleted.

Get this wrong and you have quietly damaged an archive of family correspondence
that someone will want to still have at twenty-five.

Threading headers (§3.5) exist entirely for this moment.

**Telemetry does not graduate.** The kipu store is purged at handover, not
transferred. The archive that survives to adulthood is correspondence with
family — not a record of where its owner was at fifteen.

Positions the child *chose* to attach to letters (§4.2) do graduate, because
they are content. The distinction is the whole point: what you said is yours to
keep; what was observed about you is not kept at all.

*Incidental timing note:* T-Mobile reportedly intends to refarm most LTE
spectrum for 5G SA around 2028, with full LTE sunset near 2035. That hits
LTE-M and NB-IoT equally, so it changes no decision here — but it does put a
horizon on the hardware, conveniently about the same horizon as a kid
graduating to a normal account.

---

## 10. Development Parts List

### Core

| Part | Qty | ~Cost | Notes |
|---|---|---|---|
| Walter module + certified antennas | 2 | €140 | Not the €250 devkit |
| KeebDeck Basic | 1 | $40 | Proto + open reference footprint |
| KeebDeck Keyboard (keypad + dome sheet) | 3 | $45 | For custom PCB work |
| GDEY027T91-FL02 | 2 | ~$80 | One spare for the torn FPC |
| GDEY042T81-FL02 | 1 | ~$60 | To settle the size question |
| DESPI-C02 | 1 | $10 | EPD adapter — headers + boost rails |
| ESP32-FTS02 | 1 | $15 | Frontlight driver, skips discrete design |

### Power and measurement

| Part | Qty | ~Cost | Notes |
|---|---|---|---|
| **Nordic PPK2** | 1 | $110 | **Buy before writing any firmware** |
| LiPo 3000mAh, JST-PH | 2 | $30 | |
| MAX17048 breakout | 1 | $8 | |
| Hologram SIM | 2 | — | Second is coverage insurance |

The PPK2 is not optional. The entire design rests on µA-level sleep claims and
a USB power meter cannot see them.

### Consumables

Logic analyser clone (~$15) · 1000µF low-ESR + 10µF ceramics · 24-pin 0.5mm FPC
extension cables · JST-PH pigtails · breadboard and jumpers · u.FL–SMA pigtail
and LTE stub antenna.

**Rough total: $500–600.**

### Before any of it

Cut a 90 × 150mm piece of cardboard and put it in a kid's pocket for a week.
Cheapest experiment in the project, and it settles the 2.7"-vs-4.2" question
better than the panels will.

---

## 11. Open Questions

**Blocking hardware decisions**

- [ ] 2.7" vs 4.2" panel — decide by cardboard mockup, then by holding both
- [ ] Charger IC selection, on battery-side quiescent (BQ24075 vs BQ25185)
- [ ] Confirm MAX17048 hibernate mode current in practice
- [ ] Low-temperature panel variant — ask Good Display whether one exists for
      either size. 0–50 °C is standard-temp and a kid waits outside in January.
- [ ] Measure residual ghosting after a long partial-refresh session, at an
      angle, in bright light. Tune the clear waveform pass count against it —
      this is a privacy test, not a cosmetic one.

**Verify with vendors before committing money**

- [ ] Is the GM02SP validated on Hologram's US LTE-M carriers? Ask both.
      Modem/MVNO mismatches cost a month.
- [ ] Do PSM/eDRX timers survive carrier negotiation? They are *requested*, not
      guaranteed. PPK2 territory.
- [ ] Is a modem RING/interrupt line routed to an RTC-capable pin on Walter?
      (Optimisation, not a dependency.)
- [x] ~~Confirm Walter's 9.8µA test conditions — WiFi genuinely powered down?~~
      Resolved: WiFi and BLE are not compiled in (§6.3), so the radios are never
      initialised. Still worth measuring the real figure on the PPK2.
- [ ] KeebDeck stock across Lectronz, Pimoroni, Makekit. Buy the full run.
- [ ] GDEY027T91-FL02 lead time — the -FL02 bonded SKU is the only path.

**Policy, to decide and disclose**

- [ ] Guardian read access — confirm and state it on the device
- [x] ~~Can the child disable position reporting?~~ Resolved: no continuous
      precise mode exists at all (§4.2); coarse serving-cell is opt-out with
      symmetric notification (§3.7).
- [ ] GNSS — disable in firmware, or enable and tell the kid
- [ ] PIN on wake from cover screen? Protects a picked-up device; costs friction
      and a forgettable secret
- [ ] Waiting-letter count on the cover screen — useful, but it is something
      another person can start a conversation about

**Deferred**

- [ ] Put-away and sync key placement on the KeebDeck top row — which physical
      positions, and how to make put-away findable by touch (§4.1)
- [ ] ESP Web Tools flashing flow — test it end to end with someone
      non-technical before relying on it
- [ ] Measure a real GNSS cold-fix cost and time-to-fix on the PPK2 (§4.2).
      Also measure a *failed* indoor attempt — it sets the timeout before
      Chaski tells the child it couldn't find them.
- [ ] Wording of the tier-2 change letters. Two audiences, one event, and the
      child's version has to be readable by a child.
- [ ] Can the GM02SP report **neighbouring** cells, not just the serving cell?
      Determines whether rank 2 of the degradation ladder exists at all (§4.2).
- [ ] Size of the OpenCelliD US subset once imported to SQLite, and how often
      to refresh it on Wasi.
- [ ] Check tower-derived accuracy against known locations in the areas the
      child actually moves through — including at least one rural one, where
      the honest answer may be "a county".
- [ ] Youth-initiated contact requests (greyed out pending approval)
- [ ] Per-contact inbound aliases
- [ ] Greyscale photo attachments from family — would render fine on e-ink
