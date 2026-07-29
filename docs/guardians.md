# Using the Wasi web site

This is the guide for whoever manages the contact list and reviews mail on
behalf of the person carrying the device. It assumes nothing about
technical background — if you can use online banking, you can use this.

You'll have a web address (something like `https://your-address:8444`) and
a sign-in name and password, set up when the device was configured. Bookmark
the address. If you're on the home network the device's server lives on, or
connected through a VPN if one was set up for you, it should just load.

## Signing in

Enter your name and password. After five wrong attempts in a row, the site
makes you wait before trying again — that's deliberate, and it applies even
if the attempts weren't yours.

If you've forgotten your password, someone with access to the server itself
can reset it for you — ask whoever set the device up. Nobody signed in to
the web site (including you, for another guardian's account) can reset
someone else's password from inside the site. That's also deliberate: it
means one guardian can never lock another guardian out.

## Contacts

This is the list of people the device can send letters to and receive
letters from. Only guardians can add or remove someone — the young person
carrying the device can rename, reorder, or pick a picture for someone
already on the list, but cannot add or remove anyone themselves.

### Adding someone

Enter their name and email address. They're immediately able to send and
receive letters, and a short letter announcing the change is delivered to
the device — so the young person always knows who was added and never
discovers it by surprise.

The list has a maximum size (shown on the page). Removed contacts still
count against it, for reasons explained next.

### Removing someone

Removing someone does **not** delete anything. It stops future letters —
they can't send new ones in, and the device can't send new ones out to
them — but does not touch anything already sent or received. A letter
announcing the removal is delivered to the device, same as when someone is
added.

**Read this before you remove someone for a safety reason.** If you're
removing a contact because of something that happened — not just because
they've drifted out of touch — you should know exactly what this system can
and cannot do for you:

> Removing a contact does not remove the letters they already sent. Those
> stay readable, both on the device and in the mailbox, and this system
> cannot take them back. The letters live in a mailbox you already control
> — hiding them on the device would just make the device disagree with
> your own records, which is worse than the alternative. If you need those
> old letters to actually be gone, that has to happen in the mailbox
> itself, outside this site.

This is a designed limit, not an oversight. Every letter this device has
ever delivered also exists in the family's own mailbox, exactly as it does
for anyone using ordinary email — this system was built so that archive
survives to adulthood, unedited. Making removal also erase history would
mean sometimes rewriting that archive after the fact, which the whole
design refuses to do. Better to know this now, calmly, than discover it
mid-argument.

### Bringing someone back

Restoring a removed contact re-opens the channel: they can send and receive
letters again, under the same name and history as before — they don't come
back as a new person with an empty past. Another announcement letter goes
to the device.

### Changing someone's address

If someone gets a new email address, update it here rather than removing
the old contact and adding a new one — that keeps their whole letter
history under one name instead of splitting it in two. The old address is
kept on record (so old letters still show the right sender), and this
change is announced to the device too, same as an add or removal:
repointing an existing contact to a different address is exactly the kind
of change that should never happen quietly.

## Held Messages

Mail from anyone not on the contact list — a stranger, or someone who was
removed — doesn't reach the device. It waits here instead, for you to look
at.

Each held message shows who it's from and its subject, so you have enough
to decide. There are three situations, and each has one button:

| Who it's from | The button does |
|---|---|
| Someone not on the list at all | **Add as contact, then release** — adds them (announced, as above) and delivers this message |
| Someone previously removed | **Restore contact, then deliver** — restores them (announced) and delivers this message |
| Someone already active | Delivers the message; nothing about the contact list changes, and nothing is announced, because nothing changed |

### Delivering one old letter without reopening a channel

Sometimes you want a removed contact's message to reach the device without
putting them back on the list for good — say, a one-off letter from someone
you'd rather not have sending regularly. There's no single button for
"deliver but don't restore," on purpose: silently delivering mail from
someone who can't otherwise write to the device would be exactly the kind
of change this system refuses to make invisibly. Instead, do it in three
deliberate steps:

1. **Restore** the contact (from Held, or from Contacts) — this is
   announced to the device, as always.
2. The message is now deliverable like any other; release it.
3. **Remove** the contact again — also announced.

The device sees two more announcement letters than a straightforward
release, and that's the honest cost of doing this without a hidden
override. It's a few extra clicks, not a workaround.

Held messages don't expire or get cleaned up automatically — they wait
until you act on one.

## Changes

A running log of every contact-list change: who was added, removed,
restored, or had their address updated, when, and who did it. Address
changes show both the old and new address — the only other place on this
site an address is shown, besides the Contacts page itself. This is your
record for "wait, who changed this and when," and it's never edited or
trimmed by the software.

## Home

The dashboard shows:

- **When the device last checked in**, and its most recently reported
  battery level, signal strength, network type, and firmware version — a
  quick answer to "is the thing in her bag still alive."
- **Recent deliveries** — a list of ids and whether each one sent
  successfully, with no subject or content shown. It answers "did that
  letter go," never "what did it say."
- **Remaining message credit**, if the SMS notification feature is set up
  with a provider that reports it.

If the device hasn't checked in yet, or a figure has never been reported,
the panel says so rather than showing a stale or made-up number.

## Settings

Everything configured in the server's own configuration file is shown here,
**read-only**, along with the file's location. This site is deliberately
never allowed to write to that file — two different things writing to the
same file is exactly how configuration gets silently corrupted — so if
something here needs to change, it has to be edited on the server itself
by whoever manages it, not from this page.

## Guardians

Your own account, and any other guardians who can sign in. From here you
can:

- **Change your own password.** You'll need to re-enter your current one
  first. Changing it immediately signs out every other browser or device
  signed in as you — including, if someone else has your password without
  your knowledge, them.
- **Add another guardian.** Anyone you add here has the same access you
  do: they can add, remove, and restore contacts, and review Held mail.

## What to tell new relatives

Letters are capped at 500 characters in both directions. A letter longer
than that from a relative isn't rejected and isn't bounced back to them —
it's delivered to the device cut off at 500 characters, with a small marker
showing it was trimmed. The full letter is never lost; it's sitting
complete in the mailbox for anyone to read there. But the sender is never
told any of this happened — there's no "your letter was cut off" reply,
because generating one would mean the system emailing someone on the
child's behalf, which it deliberately never does.

The practical upshot: when you're introducing the device to a new relative,
tell them to **keep it postcard-length**. A short, complete letter every so
often reads much better on the device than half of a long one.
