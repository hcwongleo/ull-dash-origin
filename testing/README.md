# Diagnostic harnesses

Ad-hoc scripts used to find and prove the defects fixed in `origin/`. Kept
because **two of the bugs were only ever found by these, not by the unit tests**:
a per-manifest accounting drift, and readers left parked on reclaimed files.
Neither is visible to a test suite or a short benchmark.

| Script | What it demonstrated |
|---|---|
| `soak.sh` | Long-run ingest. Found the accounting drift and the parked readers. |
| `leakproof.py` | Half-open ingest connections leaking a socket and goroutine each. |
| `chunktest.sh` | Whether a chunked PUT survives the path byte-for-byte. |
| `holdtest.sh` | Behaviour of a GET for a not-yet-present segment. |

They assume an origin on `localhost:9094` and an admin listener on `:9095`.
Written against live infrastructure that no longer exists, so treat paths and
stream names as needing edits rather than as runnable.
