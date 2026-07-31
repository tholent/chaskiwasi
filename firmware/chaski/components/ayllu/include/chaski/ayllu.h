// Package ayllu is the device's contact view: the server's snapshot plus the
// youth's device-local cosmetic overlay (client spec §4.4, decision B.3).
//
// The split matters. The server owns MEMBERSHIP — who is on the list, their
// real name, and whether they are active — and nothing the child does here can
// change it (I-4: the guardian owns the list). The child owns DECORATION —
// nickname, order, pinning, portrait — and that never leaves the device: there
// is no device->server mutation on this wire, and adding engagement state to
// the server is a spec reversal (server §1.1).
//
// Portability: POSIX file APIs only, no esp_* (implementation-plan rule 3).
#pragma once

#include <cstddef>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include "chaski/wire.h"

namespace chaski::ayllu {

// Overlay is the child's decoration for one contact. Every field is optional:
// an unset field means "use the server's value", which is what makes a
// guardian's rename still show up (client §4.4).
struct Overlay {
  std::optional<std::string> nickname;
  std::optional<bool> pinned;
  std::optional<int> order;
  std::optional<std::string> portrait;
};

// Contact is the merged view the UI renders: server truth with the overlay
// applied on top.
struct Contact {
  std::string id;
  std::string name;      // overlay nickname when set, else the server's name
  std::string server_name;  // always the guardian-set name, for the settings UI
  bool active = false;
  bool pinned = false;
  int order = 0;
  std::string portrait;
};

// Store holds the last ayllu snapshot and the overlay, each persisted
// separately: a snapshot replacement must never clobber decoration, and
// clearing decoration must never touch membership.
class Store {
 public:
  virtual ~Store() = default;

  // ApplySnapshot replaces membership wholesale. It is called only when the
  // response carries an ayllu block, i.e. when the version changed (server
  // §4.3). Contacts that disappear from the snapshot keep their overlay rows —
  // harmless, and it means a re-add restores the child's decoration.
  virtual bool ApplySnapshot(const wire::Ayllu& a) = 0;

  virtual int Version() const = 0;

  // Merged returns every known contact, tombstones included, in the child's
  // order (pinned first, then overlay order, then name). Rendering a letter
  // from a tombstone is REQUIRED — "you can still read Rosa's old letters"
  // (server §7.2) — so this list is what the inbox resolves names against.
  virtual std::vector<Contact> Merged() const = 0;

  // Composable returns only contacts the child may write to: active, and never
  // c_sys. This is the compose picker's list (client §11.2). The system
  // contact ships as a tombstone precisely so it lands here correctly with no
  // special case (server A.15).
  virtual std::vector<Contact> Composable() const = 0;

  // Lookup resolves a contact id for rendering. A miss is NOT an error and the
  // caller must not drop the letter: an unknown contact id renders under a
  // neutral fallback label and the letter is kept (client §4.4, C-14).
  virtual bool Lookup(const std::string& id, Contact& out) const = 0;

  // SetOverlay persists one contact's decoration. Unknown ids are accepted:
  // decoration may legitimately precede a snapshot that reintroduces someone.
  virtual bool SetOverlay(const std::string& id, const Overlay& o) = 0;

  virtual bool ClearOverlay(const std::string& id) = 0;
};

// FallbackLabel is what the UI shows for a contact id with no entry — never a
// raw id, never an empty sender, and never a dropped letter (C-14). The string
// itself lives in strings.c; this returns its key.
const char* FallbackLabelKey();

std::unique_ptr<Store> Open(const std::string& root);

}  // namespace chaski::ayllu
