# Security Policy

## Report Privately

Do not open a public issue for a suspected vulnerability.

Use **Security > Report a vulnerability** in the GitHub repository. If GitHub
Security Advisories are unavailable, email `hello@contextlattice.io` with the
subject `ContextLattice security report`. Do not include live credentials,
customer memory, or private keys in the first message.

Include the affected release, lane, deployment profile, reproduction boundary,
impact, and any safe proof artifact. Reports are acknowledged and triaged before
public disclosure is coordinated.

## Supported Versions

Security fixes target `main` and the latest stable release. Older immutable
release artifacts remain historical evidence and are not silently rewritten.

## Security Boundaries

Relevant reports include authentication or entitlement bypass, cross-workspace
access, unsafe secret handling, release/provenance substitution, public/private
lane leakage, memory disclosure, and unbounded local execution.
