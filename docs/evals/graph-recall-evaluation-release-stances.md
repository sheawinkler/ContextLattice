# Graph-recall evaluation release stances

The durable evaluation-cleanup marker index is content addressed by the
canonical cleanup reference. Markers are exact-once custody evidence and are
never compacted or deleted automatically; the bounded receipt is only a hot
replay window. This retention rule prevents a later replay from being counted
again after the receipt window rolls forward.

The index exposes `marker_count`, `marker_bytes`, `max_marker_count`,
`max_marker_bytes`, `state`, and `limit_reason` in the durable continuation
snapshot. Production defaults are 100,000 markers and 64 MiB of marker bytes.
When either limit would be exceeded, the state becomes `limit` and cleanup
fails closed before unlinking the queue job. Existing markers remain readable
for deduplication; operators must expand or replace the storage tier through
an explicit migration before new evaluation cleanup can proceed. The native
operator operation is `POST /ops/evaluation-cleanup/marker-cap-migration`; it
fails closed unless the deployment configures
`CONTEXTLATTICE_EVALUATION_CLEANUP_MIGRATION_CAPABILITY` and the caller
supplies that capability through the mandatory constant-time
`X-ContextLattice-Evaluation-Cleanup-Capability` header, plus non-empty
`X-ContextLattice-Operator-Principal` and `X-ContextLattice-Workspace-ID`
headers. The request still carries the closed operation fields
`authorization: gateway-go-operator`, `native_owner: gateway-go`, a
non-empty `operator_ref`, and `reason`; `extend` additionally requires an
`expected_generation` compare-and-swap value. A local operator invokes the
same route with `contextlattice marker-cap-migration`, never by editing marker
storage directly. The gateway inventories every marker through verified
handles, writes a content-addressed immutable plan and receipt, and atomically
advances an active generation pointer before publishing the larger cap. The
absolute recovery bound is 1,000,000 markers and 512 MiB. A prepared plan
without a receipt is not active and must be explicitly rolled back. Repeated
monotonic extensions are supported; a committed extension can be rolled back
only through the same authenticated principal/workspace when a fresh pre/post
inventory proves every existing marker still fits the prior generation. No
operation deletes markers or queue custody.

The active pointer, immutable migration records, and rollback receipt are
replayed at startup before the manifest is trusted, so a crash between receipt
and pointer/manifest publication cannot lose the extension or count a marker
twice. A malformed,
unavailable, or ambiguous index similarly becomes `unavailable` and retains
pending custody.

Marker and shard creation persists every newly-created ancestor directory,
then the marker, and startup reconciles a pending index transaction. Reads are
descriptor-bound (`openat`/`O_NOFOLLOW` on Unix and a reparse-point-verified
handle on Windows) so replacement and symlink paths cannot redirect marker
identity evidence.
