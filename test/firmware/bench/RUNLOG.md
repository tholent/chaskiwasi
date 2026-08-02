# Bench run log

One line per bench run, newest last. **A run that is not written down did not
happen.**

This file is the bench tier's substitute for CI history. CI has no hardware, so
nothing automatic can tell you when these rows last actually passed, on which
firmware, or against which board. Without a log the honest answer to "does C-1
pass?" is always "someone said it did once" — which is the same answer as "no".

## How to add a line

After a bench run, append the date, the firmware sha, the board, and the
result. Record failures too: a row that regressed on a known build is worth
more than a gap.

```sh
git rev-parse --short HEAD          # the firmware sha for the run
```

## Conventions

- **Firmware sha** is the commit the flashed image was built from. If the tree
  was dirty, say so — `abc1234-dirty` — because a run against uncommitted code
  cannot be reproduced.
- **Result** is `pass`, `fail (C-n, ...)`, or `partial (skipped: ...)`.
- Note anything about the physical setup that a later reader would need to
  reproduce the run: cable, power source, whether a SIM was fitted.

## Runs

| Date | Firmware sha | Board | Result | Notes |
|---|---|---|---|---|
| — | — | — | — | No bench run has happened yet: no Walter board has arrived. The suite compiles and skips. |
