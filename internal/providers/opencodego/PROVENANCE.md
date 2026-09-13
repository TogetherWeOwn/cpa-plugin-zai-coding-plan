# OpenCode Go module provenance

This module was written from scratch for the clean-room OpenCode Go slice.

Implementation inputs were limited to:

- the first-party fixture corpus's `spec/OBSERVED-BEHAVIOR.md` and its cited Case D01, I01, I02, and I03 files;
- the merged `internal/providermodule.Module` interface and coordinator contracts in this repository at `7272eec45925`;
- the pinned CLIProxyAPI SDK public types used by that interface.

No third-party OpenCode Go plugin source tree, source file, test, fixture, identifier sequence, or prose was opened, cloned, read, adapted, or transcribed while implementing this module.

Observed behavior used by the implementation:

- `spec/OBSERVED-BEHAVIOR.md` §2a / Case I01 and I03: a 429 body names the governing quota window and `Retry-After` carries the matching reset countdown;
- `spec/OBSERVED-BEHAVIOR.md` §2b / Case I02: the 403 doctype response with a 1–3 second countdown is a transient microthrottle and does not modify quota-window pacing state;
- `spec/OBSERVED-BEHAVIOR.md` §4 / Case I02: proxy-visible connection IDs can be grouped as one configured logical account when supplied by the operator;
- `spec/OBSERVED-BEHAVIOR.md` §1 / Case D01: the module does not depend on an authenticated dashboard payload.

The five-hour and weekly enforcement shapes were not observed. They are deliberately limited to the issue-mandated structural generalization of the observed monthly pattern: a 429 that explicitly names the window plus a bounded reset hint. The module status exposes this limitation. When a quota response has no reset evidence, the module keeps `resets_at` unknown and uses a separate conservative impairment cooldown; it does not publish the cooldown as a reset timestamp. Unobserved utilization is likewise omitted rather than reported as zero.

Review gate:

```sh
git grep -iE '<prohibited-author>|<prohibited-upstream-repository>' -- internal/providers/opencodego
```

The reviewer substitutes the prohibited names from the issue's binding clean-room gate. The command must return no matches.
