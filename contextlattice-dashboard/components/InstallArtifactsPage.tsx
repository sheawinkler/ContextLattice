"use client";

import { useState } from "react";

type CommandStep = {
  label: string;
  command: string;
};

const INSTALL_STEPS: CommandStep[] = [
  {
    label: "01 // clone",
    command: "git clone https://github.com/sheawinkler/ContextLattice.git",
  },
  { label: "02 // enter", command: "cd ContextLattice" },
  { label: "03 // configure", command: "cp .env.example .env" },
  { label: "04 // launch", command: "gmake quickstart" },
];

const VERIFY_STEPS: CommandStep[] = [
  { label: "doctor", command: "contextlattice doctor --pretty" },
  { label: "runtime health", command: "curl -fsS http://127.0.0.1:8075/health | jq" },
  { label: "local monitor", command: "gmake monitor-open" },
];

const ADOPT_STEPS: CommandStep[] = [
  { label: "detect", command: "contextlattice_adopt status --pretty" },
  {
    label: "integrate",
    command:
      "contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty",
  },
  {
    label: "verify",
    command:
      "contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --check --pretty",
  },
];

const INSTALL_COMMANDS = INSTALL_STEPS.map((step) => step.command).join("\n");

const AGENT_PROMPT = `Install the open-source ContextLattice repository from https://github.com/sheawinkler/ContextLattice using its documented quickstart. Run contextlattice doctor --pretty, then integrate the current repository only with agent harnesses already installed on this machine. Verify the managed integration with contextlattice_adopt integrate --check, do not install third-party agent CLIs, and report the exact commands plus any skipped integrations.`;

function CopyButton({ value, idleLabel = "Copy" }: { value: string; idleLabel?: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    await navigator.clipboard.writeText(value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1200);
  }

  return (
    <button className="cl-button" type="button" onClick={copy}>
      {copied ? "Copied" : idleLabel}
    </button>
  );
}

function CopyableCommand({ label, command }: CommandStep) {
  return (
    <div className="cl-command-card">
      <div>
        <span className="cl-label">{label}</span>
        <code>{command}</code>
      </div>
      <CopyButton value={command} />
    </div>
  );
}

export function InstallArtifactsPage() {
  return (
    <div className="cl-page cl-install-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Install // OSS local runtime</p>
          <h2>Start local. Stay in control.</h2>
          <p>
            Clone the public repository, launch the local stack, and drive ContextLattice from the CLI. No dashboard account is required.
          </p>
        </div>
        <aside className="cl-overview-stamp" aria-label="Installation profile">
          <span className="cl-label">primary interface</span>
          <strong>CLI</strong>
          <span>Open source / local-first / Compose v2</span>
        </aside>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">fast path</p>
            <h3>Clone, configure, launch</h3>
          </div>
          <CopyButton value={INSTALL_COMMANDS} idleLabel="Copy all" />
        </div>
        <p className="cl-panel-note">
          Requires a Compose v2-compatible container runtime plus <code>gmake</code>, <code>jq</code>, <code>rg</code>, <code>python3</code>, and <code>curl</code>.
        </p>
        <div className="cl-command-grid">
          {INSTALL_STEPS.map((step) => (
            <CopyableCommand key={step.label} {...step} />
          ))}
        </div>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">runtime truth</p>
            <h3>Verify before integrating</h3>
          </div>
          <a
            className="cl-text-link"
            href="https://github.com/sheawinkler/ContextLattice#quickstart"
            target="_blank"
            rel="noreferrer"
          >
            Read quickstart
          </a>
        </div>
        <div className="cl-command-grid">
          {VERIFY_STEPS.map((step) => (
            <CopyableCommand key={step.label} {...step} />
          ))}
        </div>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">agent adoption</p>
            <h3>Wire only what is installed</h3>
          </div>
        </div>
        <p className="cl-panel-note">
          Run these commands from the repository you want to integrate. Detection skips unsupported or missing agent harnesses.
        </p>
        <div className="cl-command-grid">
          {ADOPT_STEPS.map((step) => (
            <CopyableCommand key={step.label} {...step} />
          ))}
        </div>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">agent prompt</p>
            <h3>Hand the setup to an agent</h3>
          </div>
          <CopyButton value={AGENT_PROMPT} idleLabel="Copy prompt" />
        </div>
        <pre className="cl-prompt-box">{AGENT_PROMPT}</pre>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">local surfaces</p>
            <h3>What the quickstart provides</h3>
          </div>
        </div>
        <div className="cl-surface-grid">
          <article><strong>CLI lifecycle</strong><span>context, resume, remember, correct, finish, and doctor</span></article>
          <article><strong>Local dashboard</strong><span>runtime, retrieval, session, and memory visibility on your machine</span></article>
          <article><strong>Agent adapters</strong><span>managed instructions and hooks for detected harnesses</span></article>
          <article><strong>Durable memory</strong><span>project-scoped writes, staged retrieval, and bounded continuation</span></article>
        </div>
      </section>
    </div>
  );
}
