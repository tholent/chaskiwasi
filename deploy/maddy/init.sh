#!/bin/sh
# init.sh — provisions the one test mailbox this fixture needs, once.
#
# Runs as a one-shot compose service using the maddy binary itself in CLI
# mode (no server process involved — the CLI opens the same SQLite state
# directly). Idempotent: every step checks first and skips on a second run,
# so restarting the stack against an existing volume doesn't fail init.
#
# MADDY_TEST_USER / MADDY_TEST_PASSWORD come from the environment (see
# ../compose.dev.yml); they are also what deploy/wasi.example.toml's
# mail.address and the WASI_IMAP_PASSWORD secret are set to, so Wasi, this
# script, and a human poking at the stack all agree on one account.
set -eu

USER="${MADDY_TEST_USER:?MADDY_TEST_USER must be set}"
PASSWORD="${MADDY_TEST_PASSWORD:?MADDY_TEST_PASSWORD must be set}"

if maddy creds list | grep -qxF "$USER"; then
    echo "init: credentials for $USER already exist"
else
    maddy creds create -p "$PASSWORD" "$USER"
    echo "init: created credentials for $USER"
fi

if maddy imap-acct list | grep -qxF "$USER"; then
    echo "init: storage account for $USER already exists"
else
    maddy imap-acct create "$USER"
    echo "init: created storage account for $USER"
fi

# INBOX, Junk, Trash, Sent, Drafts, Archive are created automatically with
# the storage account. Held is Wasi's own quarantine folder (§5.1, §8) and
# is not one of maddy's defaults, so it needs creating explicitly.
if maddy imap-mboxes list "$USER" | grep -qE '^Held\b'; then
    echo "init: Held mailbox already exists for $USER"
else
    maddy imap-mboxes create "$USER" Held
    echo "init: created Held mailbox for $USER"
fi

echo "init: done"
