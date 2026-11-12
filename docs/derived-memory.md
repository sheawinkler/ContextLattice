# Keep The Summary Bound To Its Source

An ordinary citation says "read this too." A derivation says "this conclusion
depends on this exact input." They are not interchangeable.

Use the primary CLI to retrieve the source and retain its owner-issued `event_id`
and `content_hash` from that same observation. Generate the summary from that
input, then register the dependency with the existing write command:

```sh
contextlattice write --project example --file notes/summary.md \
  --content-file summary.md \
  --relations-json '[{"target_id":"example::notes/source.md","relation":"derived_from","target_event_id":"<observed-event-id>","target_content_hash":"<observed-content-hash>"}]'
```

Replace both observed fields with the exact input identity; do not invent them
from a filename or timestamp. Registration rejects stale pins, cross-project or
self dependencies, exact-state mirrors, excluded sources and unavailable custody.
If the input changes before the write, retrieve it again and regenerate the
summary. Even identical summary text receives a new event when its pinned input
changes. An unchanged exact retry remains idempotent.

The CLI checks `derivedMemory` support on the existing capability endpoint before
writing and requires an explicit custody receipt afterward. An older or unready
gateway is not silently accepted. A missing receipt after submission means the
write may already exist, but its protection is unconfirmed; do not blindly retry.
The receipt confirms registration, not factual truth or current serving eligibility.

New native retrieval/context packets check original closed reference custody and
current owner versions before serving a registered derivative. A corrected,
quarantined, missing or unverifiable input blocks the affected derivative; it does
not erase its history or taint ordinary citation-only neighbors. Dependency
traversal is bounded. Missing receipts, cycles, exhausted bounds and incoherent
owner state fail closed rather than becoming "no dependencies."

Context and synthesis compilation retain the first owner observation, including
through graph enrichment, persistence fallback and final compact/full projection.
Source drift can return `derived_memory_source_unavailable`; recompute against
current inputs rather than repeatedly resubmitting old pins. Safety omissions are
reported as omissions, not mislabeled backend failures.

Graph repair can refresh ordinary citations. It cannot regenerate a summary or
silently bind old computation to a newer source. Recompute and write a new summary
event to restore eligibility. No model execution is started automatically.

This is same-project, explicit local-memory derivation support, not a universal
revocation system. It does not retroactively recall packets already delivered,
replace historical/raw reads, or automatically register every existing summary,
temporal claim, preference, skill or prepared context. Provenance checks establish
input identity and custody, not factual truth, transformation correctness,
execution permission or independently demonstrated task efficacy.
