# External Provider Authentication Boundary

Decision date: 2026-08-01.

Pi and OpenCode are optional external runner surfaces. ContextLattice validates
their installed non-interactive CLI contracts without assuming that either tool
is authenticated or entitled to a paid model.

## Discovery versus execution

The provider smoke is network-free by default:

```zsh
scripts/agent/audit-external-provider-cli-smoke \
  --providers pi,opencode \
  --pretty
```

It resolves each executable, reads version/help output, and validates the
non-interactive surface. It does not send a prompt, create retrieval seed data,
or prove provider billing access.

Real provider calls require the explicit `--execute` flag and, where needed, an
explicit `--provider-model provider=model` selection. Retrieval injection and
tool-retrieval modes additionally require `--execute`. Do not add `--execute` to
default adoption, doctor, or release gates.

## Pi

The installed Pi CLI exposes `--provider`, `--model`, and `--api-key`, documents
`OPENAI_API_KEY`, and accepts OpenAI model selection. ContextLattice execution
uses `--print --mode text --no-session --no-tools --no-skills
--no-context-files` for the no-tool smoke. `--no-session` prevents the smoke from
creating reusable Pi session history.

This CLI contract proves configuration support; it does not prove that an API
key is present, valid, funded, or authorized for a particular model.

## OpenCode

The installed OpenCode CLI exposes provider list/login/logout commands, model
selection in `provider/model` form, and `run --pure`. ContextLattice execution
uses an isolated temporary working directory and `run --pure --format default`.
Pure mode suppresses external plugins, but the CLI may still read its own global
provider configuration.

ContextLattice must not run provider login/logout, overwrite OpenCode config, or
reuse another harness's browser/OAuth session as an implicit setup step.

## Credential and data rules

- Each provider CLI owns its own credentials, config, cache, and session state.
- Never copy or reinterpret ChatGPT, Codex, browser, Keychain, or another CLI's
  session as provider credentials.
- Never print API keys or credential-file contents in smoke output.
- A paid provider call is account activity and must be explicitly authorized.
- Keep execution in the isolated workdir unless an operator explicitly requests
  repository context. Pi additionally stays sessionless.
- A discovery pass may establish adapter readiness. Claims about actual model
  access, response quality, cost, or account compatibility remain unproven until
  an explicitly authorized execution artifact exists.
