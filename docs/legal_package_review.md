# Legal Package Review (Launch Baseline)

Updated: 2026-02-18

This review summarizes legal-facing launch artifacts and dependency/license
signals. It is not legal advice.

## Package status

- License baseline: `LICENSE` (BSL 1.1 with additional use grant).
- Commercial baseline: `docs/legal/COMMERCIAL_LICENSE.md`.
- Hosted terms: `docs/legal/TERMS_OF_SERVICE.md`.
- Privacy notice: `docs/legal/PRIVACY_POLICY.md`.
- DPA baseline: `docs/legal/DPA.md`.
- Acceptable use: `docs/legal/ACCEPTABLE_USE_POLICY.md`.
- Subprocessors list: `docs/legal/SUBPROCESSORS.md`.

## External legal/regulatory anchors reviewed

- BSL 1.1 canonical text:
  - https://mariadb.com/bsl11/
- GDPR transparency obligations:
  - https://gdpr-info.eu/art-13-gdpr/
- California privacy rights overview (CCPA/CPRA):
  - https://oag.ca.gov/privacy/ccpa
- FTC privacy and data security guidance:
  - https://www.ftc.gov/business-guidance/privacy-security

## Dependency license posture

- Current core stack dependencies are predominantly permissive licenses (MIT,
  BSD, Apache, ISC), based on prior package review.
- Keep release-time SPDX inventory as a required step before GA.

## Outstanding legal actions before enterprise GA

- Counsel review of all docs in `docs/legal/`.
- Jurisdiction-specific addenda (if selling outside US baseline).
- Final commercial order form language (liability caps, indemnity, SLA).
- Confirm any public claims match actual data residency/retention behavior.
