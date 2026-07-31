# Local patches to vendored utf8proc

Vendored version: **2.8.0** (Unicode **15.0.0**), from
<https://github.com/JuliaStrings/utf8proc>, MIT — see `LICENSE.md`.

Patched vendor code is a trap on upgrade, so every local change is recorded
here. **Re-apply or re-verify each one when bumping the version**, and check
first whether upstream has already fixed it.

## P-1 · `last_boundclass` is `utf8proc_int32_t*`, not `int*`

Files: `utf8proc.c` (2 declarations), `utf8proc.h` (1 declaration).

`utf8proc_decompose_char` and `seqindex_write_char_decomposed` declared the
parameter as `int *last_boundclass`, but pass it to
`grapheme_break_extended(..., utf8proc_int32_t *state)`.

Those two types are identical on x86-64 Linux, where `int32_t` is `int` — so
the host build is clean. On xtensa (ESP32-S3, newlib) `int32_t` is `long int`,
so the call is a pointer-type mismatch and ESP-IDF's `-Werror=all` rejects it:

```
utf8proc.c:502:63: error: passing argument 3 of 'grapheme_break_extended'
from incompatible pointer type [-Wincompatible-pointer-types]
```

This is **upstream's own fix**, released in utf8proc 2.9.0; it is applied here
rather than upgrading because 2.9.0 moves to Unicode 15.1.0, which would break
lockstep with the server's `rivo/uniseg` v0.4.7 (Unicode 15.0.0) — the exact
skew decision B.7 and test C-9 exist to prevent. Upgrading utf8proc is a
deliberate, paired change with the server's segmenter, not a way to fix a
compiler error.

Dropping the patch by upgrading to ≥2.9.0 is therefore fine **only** as part of
that paired bump.

Not done, on purpose: silencing the warning for this component. The mismatch is
a genuine type error on the target, and `-w` here would have hidden it while
leaving the wrong pointer type in a parser that reads bytes off the network.
