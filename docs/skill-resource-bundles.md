# Skill Resource Bundles

`services/gateway-go/internal/skillresources` is a pure Go codec for inactive,
versioned skill resource bundles. It has no filesystem, execution, network,
gateway-route, installation, activation, or model-calling behavior. A valid
bundle or matching digest grants no permission to run anything and makes no
efficacy claim.

## API

```go
import "github.com/contextlattice/gateway-go/internal/skillresources"

bundle, err := skillresources.Decode(wireBytes, exactSkillMarkdownBytes)
err = skillresources.Validate(bundle, exactSkillMarkdownBytes)
canonicalBytes, err := skillresources.CanonicalJSON(bundle, exactSkillMarkdownBytes)
bundleDigest, err := skillresources.Digest(bundle, exactSkillMarkdownBytes)
contentDigest := skillresources.SHA256(exactResourceBytes)
```

`Decode` accepts exactly one JSON value, returns a zero `Bundle` on failure, and
verifies all content and Markdown hashes. It owns newly decoded slices.
`Validate`, `CanonicalJSON` and `Digest` also verify caller-constructed typed
values. They do not mutate inputs; callers must not concurrently mutate input
slices. Concurrent read-only calls are supported. Errors are fixed exported
sentinels usable with `errors.Is`; they never include input content, field
names supplied by callers, filesystem paths, or decoder diagnostics.

## Version 1 Contract

The envelope contains `manifest` and `resources`. All fields in the exported
types are required, including empty lists, and none can be null. Field names
are exact and case-sensitive. Unknown fields, duplicate keys (including
escaped aliases), malformed UTF-8, unpaired UTF-16 escapes, extra JSON values,
wrong types and non-integer numeric encodings are rejected. Arbitrary Go
`json.Unmarshal` is not a substitute for `Decode` on untrusted wire input.

The manifest contains:

- `schema_id`: exactly `skill_resource_bundle.v1`; `version`: integer `1`.
- `skill_sha256`: SHA-256 of the exact externally supplied `SKILL.md` bytes.
- `applicability`: ordered `conditions`, `exclusions`, and `failure_modes`.
- `environment`: `os`, `architecture`, and `fingerprint_sha256`.
- `rollback`: `identity_sha256` and ordered `steps`.
- `entrypoint`: an exact resource `path`, ordered script `arguments`, and an
  `interpreter` with `source`, `name`, `version`, and ordered `arguments`.

Environment and rollback fingerprints are opaque identity claims with strict
digest syntax. They are not recomputed from host state or fetched artifacts:
the codec has neither. A later integration must independently validate those
claims against its approved environment and reviewed rollback artifact.

Every resource contains `path`, `size_bytes`, `sha256`, and `content`. JSON
`content` is canonical padded standard base64; Go uses `[]byte`. Base64 with
ignored whitespace, nonzero pad bits or alternate alphabets is rejected.
Decoded content must be valid UTF-8. Exact byte count and SHA-256 are checked;
empty content is allowed but null is not. Line endings, BOM, whitespace,
Unicode spelling and even valid UTF-8 control bytes in resource content are
not rewritten. A resource is data, not proof of an executable program.

The interpreter source must be `installed`, never a download URL or install
instruction. Name is a lowercase ASCII identifier starting with a letter;
subsequent characters can include digits, dot, underscore and hyphen. Version
is a bounded literal starting with a digit and containing only ASCII letters,
digits, dot, plus and hyphen, not `latest`, a wildcard or a comparison range.
This identifies a runtime the operator intends to use; it does not prove that
runtime is installed, resolved to a particular executable, or trusted.
Arguments are exact ordered strings, not shell expressions to evaluate. Empty
arguments are retained. Validation does not authorize their semantics.

## Limits And Paths

- One to four resources, at most 8 KiB decoded bytes each and 32 KiB total.
- External Markdown must be nonempty valid UTF-8 and at most 128 KiB.
- JSON input is at most 128 KiB, including whitespace, and at most eight nested
  containers. Limits are checked before typed decoding. The wire budget covers
  base64 and JSON escaping at the admitted metadata/resource maxima.
- Each metadata/argument list has at most eight entries of at most 256 UTF-8
  bytes each. Conditions and rollback steps are nonempty; other lists may be
  empty but cannot be null. Descriptive entries cannot be blank. Control and
  Unicode format characters are rejected in metadata/arguments. Metadata is
  preserved, never trimmed, normalized or case-folded.
- Runtime names, literal versions, OS and architecture identifiers are at most
  64 bytes. OS/architecture use the same identifier syntax as runtime names.
- Resource paths start with exactly `scripts/` or `references/`, use slash
  separators, are at most 240 bytes, and have segments of at most 64 bytes.
  Segments start with an ASCII alphanumeric character; remaining characters
  are ASCII alphanumeric, dot, underscore or hyphen. Trailing dots, dot-only
  components, repeated/trailing separators, absolute/drive/UNC paths,
  backslashes, percent-encoding, whitespace, controls and non-ASCII path
  spellings are rejected rather than normalized.
- Reserved DOS device basenames and any case spelling of `SKILL.md` are
  rejected in all segments. Leading-dot paths, including source manifests,
  are not accepted. Case-colliding components and duplicate or file/directory
  prefix collisions are rejected across the entire resource set.
- Entrypoint must exactly match an admitted resource under `scripts/`.

This is lexical validation only. Symlinks, hardlinks, root containment, file
swaps, permissions, secrets and actual installed runtime identity cannot be
verified from this envelope. A future filesystem integration must read bounded
regular-file snapshots, reject link escapes, check open-file identity, preserve
the reviewed bytes, and prevent time-of-check/time-of-use substitution. Do not
treat this package as a sandbox or secret scanner.

## Canonical Identity

Digests use `sha256:` plus 64 lowercase hex characters. `CanonicalJSON` emits
the exact exported struct field order, no insignificant whitespace and no
trailing newline, using Go `encoding/json` escaping and standard padded base64.
Resources are sorted by bytewise ASCII path order. Only that list is sorted;
all metadata, rollback and argument list ordering is meaningful. The input
resource slice and bytes are not modified.

`Digest` is SHA-256 of those canonical JSON bytes. It covers schema/version,
the exact Markdown digest, applicability, environment/rollback identities and
details, entrypoint/runtime/arguments, and every resource path, declared size,
verified digest and exact content. Reordering resources or JSON object keys
does not change identity; changing a resource byte, Markdown byte, runtime,
argument ordering or applicability value does. Byte/hash mismatch fails rather
than creating a new identity. There is no self-referential digest field.

For cross-language producers, JSON whitespace/escape spelling is not identity
material, but decoded string/content bytes are. Use this codec's canonical
output as the initial interoperability reference. Unicode NFC/NFD, CRLF/LF and
different argv strings remain distinct; they are never normalized.

## Integration Boundary

This foundation does not change the existing Foundry, Skills Index, CLI,
receipt or output-contract registries. It does not persist bundles or receipts.
Those integrations must separately bind this complete identity before review,
holdout evaluation, export, installation, invocation and independent outcome
verification. A legacy Markdown-only digest is not a bundle digest. Discovery,
validation, export and install are not invocation or efficacy evidence.

Manual ownership, portable inactive artifacts and safety validation belong in
public core. Paid governed promotion/activation remains separately entitled.
Neither edition should execute resources automatically in the gateway. Existing
Markdown-only activation must not silently drop resources when bundles are
eventually wired into Foundry; whole-bundle installation/rollback needs its own
review or an explicit rejection gate.

Package tests cover strict admission, boundaries, byte preservation, digest
binding, path collisions, deterministic generated permutation controls and
concurrent read-only use. They establish codec behavior, not script safety,
independent outcomes, runtime compatibility, installed behavior or task efficacy.

## Offline CLI Inspection

The existing native Skills Index command can inspect a local envelope without
contacting the gateway or executing any resource:

```text
contextlattice_skills_index bundle-inspect --skill-file SKILL.md --bundle-file bundle.json --pretty
contextlattice_skills_index bundle-inspect --skill-file SKILL.md --bundle-file bundle.json --expect-digest sha256:<64-lowercase-hex>
```

`--expect-digest` optionally pins the complete canonical bundle identity, not
the raw JSON file hash or a Markdown-only identity. A mismatch fails. The CLI
uses the exact-byte codec above, including its unchanged byte/count/depth
limits. Unknown, duplicate and missing-value flags and positional arguments
fail; there is no stdin, network, install, execution or approval option.

Inspection currently supports native Linux/macOS file snapshots; other hosts
fail closed. Both inputs must be bounded regular files. Input path components
are checked for symlinks, opened descriptors are checked against the observed
file identities, and parent/leaf identities are rechecked. Descriptor-relative
traversal and nonblocking leaf open avoid waiting for a swapped-in FIFO.
Reads are bounded to the codec limits plus one byte, and exact bytes are read
again before emitting a result to detect changed content, including same-size
changes. Symlinked input paths are rejected, including temporary-directory
aliases: use the physical path. Embedded resource paths are not opened.

Success emits only a fixed inspection schema, `valid_manifest: true`, canonical
bundle and exact-file/resource digests, byte/resource counts and fixed review
rule identifiers. It never echoes script content, arguments, applicability or
rollback text, resource names or local paths. Parse/read/codec/identity/output
errors are content-free. `bundle_file_sha256` identifies the raw JSON snapshot;
it can change with JSON whitespace while `bundle_digest` remains unchanged.

The existing conservative secret-blocking and review regexes scan the Markdown,
all decoded resource UTF-8 bytes regardless of extension (including NUL), and
all manifest/resource text fields. A blocked match fails without echoing the
match. Review matches are reported only by fixed rule ID. This is a heuristic,
not an exhaustive secret or malicious-code detector. Even with no matches,
`review_required` is always true, and `active`, `execution_authorized`,
`independent_verification` and `runtime_environment_verified` are always false.
There is no auto-approval or promotion path in this command.

This is a validator, not a sandbox, runner, installation receipt or verified
runtime environment. Checks describe the captured bytes, not future path
contents, a filesystem-wide atomic snapshot, hardlink isolation or protection
against a privileged writer concealing transient changes. Neither interpreter
availability nor environment/rollback claims are verified. Fixed argv in a
manifest remains authored data, not trusted execution authority; shell text
is never evaluated. Code review and separately authorized, digest-bound
execution with its own dry-run, idempotency, timeout and redaction controls
remain required. This command grants none of that authority and supplies no
independent efficacy evidence.
