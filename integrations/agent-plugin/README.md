# ContextLattice for local agent hosts

One on-demand [recall skill](plugins/contextlattice/skills/contextlattice-recall/SKILL.md)
calls the already-installed CLI. It is not a memory provider, startup hook,
model, or remote service. The local Codex, Claude, and Droid plugins and the Prime
capability package ship that same file. Mercury's native adapter returns its
contents from a command or tool; it does **not** run a subprocess or query a
gateway from the plugin. Rust Ultra uses its own native skill loader.

## Stage and back up

Run from this directory. Choose stable, existing, absolute physical directories
for `DIST` and `BACKUPS`; keep both outside agent discovery roots. The hosts
must be able to read `DIST` after the install. Do not point a host at a
temporary checkout. Use an owner-private backup location: native settings may
contain sensitive configuration; never post these snapshots.

```sh
python3 -B manage.py install --kind marketplace --root "$DIST"
python3 -B manage.py install --kind mercury --root "$DIST"
MARKET="$DIST/contextlattice-local"
ID=contextlattice@contextlattice-local
```

Before changing a profile, select the actual host binary and its config root
(`PROFILE`). Back up **that host only**:

```sh
python3 -B backup_profile.py "$HOST" --profile "$PROFILE" --backup-dir "$BACKUPS"
```

Backup host names are `codex`, `claude-code`, `droid`, `prime-agent`, and `mercury`.
This captures the relevant native settings/registration files or records their
absence; it does not copy auth stores. The direct skill installer never edits
shared settings. Its uninstall backs up its own files.

**Preflight:** use the host's inventory below before adding anything. If this
plugin ID, marketplace name, or skill already exists, run the verifier first.
If it passes, skip installation. If the source, bytes, scope, or ownership
differs, stop; do not replace, layer a second skill, or silently enable a
disabled plugin. Keep existing hooks and instruction files untouched. Install
only one packaging mode per host. Use host uninstall only for a registration
you created. A materialized resource refuses unowned/changed targets (even
byte-identical unowned skills), symlinks, extra children and concurrent access.

## Native install

Commands assume the selected host profile environment. The verifier accepts
`--binary` to select an explicit installed executable. It returns a path-free
grade and digest, does not send a prompt, and fails on absent or mismatched
native evidence. File presence alone is not acceptance.

| Host | Inspect before install | Install | Verify | Remove |
| --- | --- | --- | --- | --- |
| Factory Droid | `droid plugin marketplace list`; `droid plugin list --scope user` | `droid plugin marketplace add "$MARKET"` then `droid plugin install "$ID" --scope user` | `python3 -B verify.py droid --profile "$HOME/.factory" --dist "$DIST"` | `droid plugin uninstall "$ID" --scope user`; remove `contextlattice-local` only if added for this plugin and unused by anything else |
| Codex | `codex plugin marketplace list`; `codex plugin list --json` | `codex plugin marketplace add "$MARKET"` then `codex plugin add "$ID"` | `python3 -B verify.py codex --profile "$CODEX_HOME" --dist "$DIST"` | `codex plugin remove "$ID"`; `codex plugin marketplace remove contextlattice-local` |
| Claude Code | `claude plugin marketplace list`; `claude plugin list --json` | `claude plugin marketplace add "$MARKET"` then `claude plugin install "$ID"` | `python3 -B verify.py claude --profile "$CLAUDE_CONFIG_DIR" --dist "$DIST"` | `claude plugin uninstall "$ID" --keep-data`; `claude plugin marketplace remove contextlattice-local` |
| Prime Intellect Rust prime-agent | `prime-agent package list`; `prime-agent skills --json` | `prime-agent package install "$MARKET/plugins/contextlattice"` | `python3 -B verify.py prime-agent --profile "$PRIME_AGENT_CODING_AGENT_DIR" --dist "$DIST"` | `prime-agent package remove "$MARKET/plugins/contextlattice"` |
| Mercury | `mercury plugins info contextlattice` (inspect output, not just exit status) | `mercury plugins validate "$DIST/contextlattice" --json` then `mercury plugins install "$DIST/contextlattice" --yes --no-setup` | `python3 -B verify.py mercury --profile "$MERCURY_HOME" --dist "$DIST"` | `mercury plugins uninstall contextlattice --yes`; do not use `--purge` |
| Rust Hermes Agent Ultra | `hermes-ultra --version`; `hermes-ultra skills list` | `python3 -B manage.py install --kind skill --root "$HERMES_HOME/skills"` | `python3 -B verify.py hermes-ultra --profile "$HERMES_HOME"` | `python3 -B manage.py uninstall --kind skill --root "$HERMES_HOME/skills" --backup-dir "$BACKUPS"` |

The variables in the table denote the **explicitly selected** host profiles;
set them before using the commands. In particular, a missing `HERMES_HOME`
does not select a Rust profile. Inspect the installed `hermes-ultra` launcher
and canonical `hermes-agent-ultra` version; a bare `hermes` can be a different
Python runtime. Do not install this skill as a Python memory provider.
If a profile's `skills` directory does not exist, create only that directory
after confirming the intended profile. Do not use a shared `.agents/skills`
root on a host that already sees the packaged skill.

Droid reads the same staged [marketplace format](https://docs.factory.com/harness/plugins);
there is no separate Droid package. Choose the plugin **or** the personal
skill, not both. A same-name shared skill can win discovery even when the
plugin is active. To check effective selection, a fresh session's
`droid.list_skills` must return one enabled, user-invocable guide whose path
and body match the registered plugin cache. There is no separate slash command.
For removal, finish any session using this plugin first. Check the user
plugin inventory and the skill in a fresh process; do not delete an entire
cache or settings file to clear a stale entry.

## Additional profiles

These opt-in resources use the skill from the **stable** receipt-managed
`$DIST/contextlattice-local`, not a source checkout. Ship the additive
`extra_profiles.py` and `gemini/gemini-extension.json` beside the durable
operator kit's existing `manage.py`; do not replace a running profile, core
marketplace, or manager. Select an existing physical profile below. The helper
creates only its native resource directory if necessary; no registry, settings,
model, hook, MCP server, package install, or network is involved.

| Host argument | Ordinary profile | Native destination beneath profile | Qualified surface |
| --- | --- | --- | --- |
| `gemini` | `$HOME/.gemini` | `extensions/contextlattice-recall/` | Direct-load Gemini extension; `extensions list` enabled it and `skills list --all` resolved its one bundled skill. |
| `gemini-skill` | `$HOME/.gemini` | `skills/contextlattice-recall/` | Native standalone skill instead of the extension; choose one. |
| `droid` | `$HOME/.factory` | `skills/contextlattice-recall/` | A clean profile's `droid.list_skills` returned the personal skill and its body. Some configured profiles select only a shared skill root; check the returned file path before calling this installed and discoverable. |
| `pi` | `$HOME/.pi/agent` | `skills/contextlattice-recall/` | One native `skill:contextlattice-recall` command in RPC `get_commands`. |
| `omp` | `$HOME/.omp/agent` | `skills/contextlattice-recall/` | One `skill:contextlattice-recall` in the native startup event `available_commands_update`. OMP 19.1.1 rejects Pi's `get_commands` request. |

For example, install in ordinary profiles **only when selected**:

```zsh
python3 -B "$KIT/extra_profiles.py" install gemini --profile "$HOME/.gemini" --dist "$DIST"
python3 -B "$KIT/extra_profiles.py" install droid  --profile "$HOME/.factory" --dist "$DIST"
python3 -B "$KIT/extra_profiles.py" install pi     --profile "$HOME/.pi/agent" --dist "$DIST"
python3 -B "$KIT/extra_profiles.py" install omp    --profile "$HOME/.omp/agent" --dist "$DIST"
```

For the corresponding row set `HOST` and `PROFILE` from the table:

```zsh
python3 -B "$KIT/extra_profiles.py" status "$HOST" --profile "$PROFILE" --dist "$DIST"
python3 -B "$KIT/extra_profiles.py" uninstall "$HOST" --profile "$PROFILE" --backup-dir "$BACKUPS"
```

`status` checks installed bytes, not host invocation. Identical **owned**
installs are no-ops; unowned targets, edited files, extra children, linked
resource roots, and same-name personal shared skills refuse installation or
removal as applicable. The two Gemini modes exclude each other. Check any
custom or project discovery roots separately. Use an owner-private backup
directory outside all discovery roots. Rollback does not require the original
distribution, removes only verified owned files, and leaves unrelated settings
and previously existing instructions alone.

To check a captured **Pi** RPC inventory, send
`{"id":"cl-inventory","type":"get_commands"}` to an ephemeral session.
For **OMP**, collect the startup inventory instead; no prompt or request is
needed. Close the session and pass its JSONL output to
`python3 verify_rpc_catalog.py pi` or `python3 verify_rpc_catalog.py omp`
on standard input. The verifier prints only the result, never the catalog.
This proves command discovery, not a model invoking the skill or reading memory.

Reproduce the Gemini and Droid loader/rollback checks without a model using
`python3 -B tests/probe_extra_profiles.py --scratch-root "$SCRATCH"` on macOS
with the hosts installed. OpenCode 2.3.2's standalone skill API starts a
private server on a TCP port, which the no-network fixture refused. Its
documented personal skill root is `$XDG_CONFIG_HOME/opencode/skills` (normally
`$HOME/.config/opencode/skills`), but an installed skill or plugin was not
qualified there. A subsequent loopback-only request reached the native API,
but its inventory did not contain the product skill. Use the CLI fallback;
no offline plugin installation or always-on service is prescribed.

## Optional project MCP

MCP is a **separate, explicit connection**. The skill/marketplace does not
register it. Use only a locally installed CLI that supports
`contextlattice mcp --project PROJECT`. Supply the selected project at
install or launch time, never a guessed default, global registration, remote
URL or token inside a committed manifest. A stdio child lives with its host.

Claude Code supports native project scope. Run within the intended project;
inspect its `.mcp.json` and existing server IDs first. Privately back up the
file if present. Use shell argument guards so an unset project cannot start a
server. Do not commit a machine-local executable path or credentials.

```sh
claude mcp add --scope project --transport stdio contextlattice -- contextlattice mcp --project "${PROJECT:?select the project first}"
claude mcp get contextlattice
claude mcp remove --scope project contextlattice
```

For Codex, use the invocation-local configuration emitted below; no
`codex mcp add` (persistent user config) or second process supervisor is
needed. Use the same argument on the session that should see the tool.

```sh
MCP_CONFIG=$(python3 -B project_mcp.py codex --project "${PROJECT:?select the project first}") || exit 1
codex -c "$MCP_CONFIG" mcp list
# In the intended working directory: codex -c "$MCP_CONFIG"
```

Claude also accepts an invocation-local JSON alternative:
`claude --mcp-config "$(python3 -B project_mcp.py claude --project "$PROJECT")"`.
Do not use both registration methods together. For a profile-only manager such
as Mercury's, use a separately selected **project-dedicated** profile or stay
CLI-only; do not add tenant-bound servers to a shared user profile. The tool
is `contextlattice_context(query)`; project is bound by the stdio launch
arguments, never chosen by a tool caller. A connection/list is not a query,
and a query is not a host lifecycle event.

For the following examples `PROJECT_PROFILE` must be a **new, project-dedicated
host profile**. Select one per host; never substitute an active shared profile.
Use the same environment prefix for the eventual host session. No registration
is made unless you explicitly run the add command:

```sh
PRIME_AGENT_CODING_AGENT_DIR="${PROJECT_PROFILE:?}" prime-agent mcp add contextlattice -- contextlattice mcp --project "${PROJECT:?}"
PRIME_AGENT_CODING_AGENT_DIR="$PROJECT_PROFILE" prime-agent mcp get contextlattice
PRIME_AGENT_CODING_AGENT_DIR="$PROJECT_PROFILE" prime-agent mcp remove contextlattice

MERCURY_HOME="${PROJECT_PROFILE:?}" mercury mcp add contextlattice --stdio contextlattice --allow contextlattice_context -- mcp --project "${PROJECT:?}"
MERCURY_HOME="$PROJECT_PROFILE" mercury mcp test contextlattice
MERCURY_HOME="$PROJECT_PROFILE" mercury mcp remove contextlattice
```

Prime `mcp get` shows a registered stdio transport, not a handshake. Mercury
`mcp test` actually launches the child and lists its tools. Do not use
`--auto-approve`. In Gemini an untrusted project refuses local MCP settings
changes; do not override trust or fall back to a global install to get a pass.

Rust Ultra's native `mcp add` accepts a **command only**, not separate child
arguments. Do not paste a shell command into that field. Choose an existing
private physical `PROJECT_TOOLS` directory for this project. The materializer
creates an exclusive, scope-bound executable; trying another project at the
same destination fails closed. It has no daemon, secrets or hardcoded host
paths and executes the trusted CLI from the selected host's PATH.

```sh
python3 -B manage.py install --kind project-mcp --root "${PROJECT_TOOLS:?}" --project "${PROJECT:?}"
HERMES_HOME="${PROJECT_PROFILE:?}" hermes-ultra mcp add contextlattice --command "$PROJECT_TOOLS/contextlattice-project-mcp/launch"
HERMES_HOME="$PROJECT_PROFILE" hermes-ultra mcp list
HERMES_HOME="$PROJECT_PROFILE" hermes-ultra mcp remove contextlattice
python3 -B manage.py uninstall --kind project-mcp --root "$PROJECT_TOOLS" --project "$PROJECT" --backup-dir "$BACKUPS"
```

Ultra writes both native JSON and YAML registrations. On the tested Rust
build its `mcp test` does not handshake with stdio: claim configuration only.
The same generated launcher was exercised by Mercury's actual stdio client.

## Rollback and proof

Remove registrations before deleting any staged source, especially Prime's
local package reference. Do not remove a marketplace another plugin uses.
Do not restore an entire settings snapshot over subsequent edits; compare the
allowlisted keys and recover only originally owned entries if native rollback
fails. For the stage, after its last consumer:

```sh
python3 -B manage.py uninstall --kind mercury --root "$DIST" --backup-dir "$BACKUPS"
python3 -B manage.py uninstall --kind marketplace --root "$DIST" --backup-dir "$BACKUPS"
```

`manage.py status` and `backup` take the same root/kind; `backup` requires
`--backup-dir`. Reinstall from the reviewed distribution or place the intact
verified backup directory back under its original resource name only when that
target is absent. A modified target or interrupted ownership receipt is a
manual reconciliation, not permission to force-remove anything.

Run `python3 -B -m unittest discover -s tests -v` for portable ownership and
rollback controls. On macOS, `tests/probe_selection.py --host codex
--scratch-root "$SCRATCH" --project "$PROJECT"` and `--host claude
--scratch-root "$SCRATCH"` use disposable profiles and an outbound-denied
sandbox with one ephemeral loopback refusal fixture. Codex lists and reads
one enabled skill and dispatches its exact command via app-server; Claude
expands the installed slash skill into its request before the refusal. Neither
test claims model-selected usefulness or successful live retrieval.

For the separate MCP client acceptance, supply a reviewed bridge executable and
its expected digest to `python3 -B tests/probe_selection.py --host codex-mcp
--scratch-root "$SCRATCH" --project "$PROJECT" --bridge-binary "$BINARY"
--bridge-sha256 "$DIGEST"`. The client lists the single tool and invokes it
on an ephemeral thread. The refusal fixture also rejects a caller project
override before another gateway request. Add `--success-fixture` for a
distinct synthetic success with a returned project-scoped witness. These
tests exercise the installed host and exact binary, not a live production
read or paid inference.

Private development can retain local receipts. Public and entitled
distributions can ship the same host payload and operator utilities unchanged;
entitlement and auth remain with the installed CLI/gateway. Do not distribute
host profiles, receipts, private policy, or test credentials. Local installation
or native validation is not third-party catalog acceptance.
