# Artifact Section Witness (Manual Shadow)

An owner can register one explicit artifact section in an existing manual
decision change. The session must bind the exact accepted decision record as
usual. Ledger acceptance is not independent approval. This path is read-only
on resume, does not discover dependent artifacts,
and does not approve, edit, notify, or execute anything. No field trial is enabled.

Add this optional field inside `decision_applicability.v1`:

```json
{
  "artifact_section_witness": {
    "artifact_input_id": "example::notes/artifact.md",
    "start_byte": 0,
    "end_byte": 24,
    "required_groups": [
      ["example::notes/source-a.md", "example::notes/source-b.md"],
      ["example::notes/required-source.md"]
    ]
  }
}
```

Every ID must be an exact reference to an original input already pinned in the
same applicability object. There is no new pin, retraction log, assumption
predicate, or separate registration. The artifact cannot also be a support.
Each support can appear only once. There must be 1-4 nonempty required groups
and the existing eight-input limit still applies. Keep group and alternative
order stable on exact retries.

The half-open byte range selects a nonblank section of at most 4 KiB from the
artifact's UTF-8 text after trimming surrounding whitespace. The selected artifact
and support files must be directly owned, current, locally readable text of at
most 32 KiB, verified against their owner receipts. A changed artifact version
cannot silently rebind the range, even when an edit appears unrelated.

Resume checks the current owner state under the existing resume fence. A whole
group is required (AND); any matching original pin within it is enough (OR).
If an alternative no longer matches, the check tries other authorized current
alternatives, never saved bytes or a newly selected replacement pin.

| Advisory Status | Meaning |
| --- | --- |
| `unchanged` | The original artifact section can still be confirmed and each declared group has at least one matching original support. |
| `revise` | The section is confirmed but at least one required group has only visible, verified owner versions that no longer match its original support pins. This calls for review, not an edit. |
| `unknown` | The target section or the support needed to decide cannot be confirmed. Hidden lifecycle rows, missing files, failed owner checks, and state drift are not evidence of a visible replacement. |

For example, a readable, current owner notice replacing one original support
can establish that the pin was lost. A withdrawal that hides the row can no
longer support the section, but on its own does not establish a visible
replacement. If a group still has a verified alternative, that alternative can
satisfy the group without revealing anything about the hidden row.

The full package uses `decision_continuity.artifact_section_witness`; the
compact package uses `provenance.decision_continuity.artifact_section_witness`.
The marker contains only `status` and `mode: manual_shadow_only`. Use the accepted
decision record for the section locator. No artifact or source text, IDs,
counts, or failure details are added to the marker. The outer decision status
does not become `verified_match` and the old action and event text are not
served from this shadow path. Matching pins assert neither support semantics
nor execution permission.

This implementation only projects the marker for an authorized session with an
explicit local scope receipt in an unbound project with available governance.
It abstains without a marker for workspace-bound or unconfirmed visibility.
Registrations without the witness retain their existing behavior. A gateway
without support rejects the extra field; do not downgrade a ledger containing a witness
without first qualifying its compatibility.

## Source Check

From the gateway module, run:

```sh
go test . -run '^(TestDecisionArtifactSectionWitness|TestAgentDecisionArtifactSectionWitness|TestAgentDecisionResume|TestDecisionApplicability)' -count=1
```

The fixtures compare normal pinned resume with the explicit shadow contract and
exercise a surviving alternative, a lost required group, unavailable owner
bytes, final session drift, unrelated writes, both package views, and durable
replay. They are isolated source checks, not model-consumption, field efficacy,
or deployment evidence.

Source evaluation ledger: the baseline is ordinary all-pin decision resume;
the measured outcomes are exact classifications, non-disclosure, and no durable
mutation. Synthetic failure cases include unavailable current evidence and
mutation during the owner-fence wait; no independent holdout was run. The focused gateway run passed in
10.963 seconds of package test time, with no provider calls or remote tools in
the fixtures. A shadow outcome reads at most eight bounded owner files; no
field latency or monetary cost was measured. Reproduce with the selector above
and the adjacent witness and decision-resume tests.
