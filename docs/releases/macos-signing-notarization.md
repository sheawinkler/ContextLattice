# macOS Signing And Notarization

ContextLattice can build the macOS launcher DMG without Apple credentials. When
Developer ID credentials are configured, the same release lane signs the app
bundles before DMG creation, signs the final DMG, submits the DMG for
notarization, staples the ticket, and runs a Gatekeeper assessment.

Developer ID certificates require an Apple Developer Program team. If the team
does not already have a Developer ID Application certificate, the Account Holder
must create one in Xcode or Certificates, Identifiers & Profiles, export it as a
password-protected `.p12`, and configure that `.p12` as a GitHub secret.

## Release Lane

The release workflow uses these scripts:

```bash
scripts/macos_import_signing_identity.sh
scripts/build_macos_dmg.sh
scripts/macos_sign_notarize_release.sh dist/ContextLattice-macOS-universal.dmg
```

`scripts/build_macos_dmg.sh` signs these app bundles inside the staging
directory before `hdiutil` creates the image:

- `ContextLattice.app`
- `ContextLattice Monitoring.app`

`scripts/macos_sign_notarize_release.sh` signs the final DMG and notarizes it
when notarization credentials are available.

## Local Credential Audit

Run this first. It prints presence/absence only, never secret values:

```bash
scripts/macos_signing_credentials_doctor.sh
```

If you have a local notarytool profile and want to validate it live:

```bash
scripts/macos_signing_credentials_doctor.sh --profile contextlattice-notary --live
```

## GitHub Secrets

Prefer the neutral secret names below for public and paid release lanes:

- `CONTEXTLATTICE_MACOS_CERT_P12_BASE64`
- `CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD`
- `CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD`
- `CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY`

Preferred notarization path: App Store Connect API key credentials:

- `CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_BASE64`
- `CONTEXTLATTICE_MACOS_NOTARY_KEY_ID`
- `CONTEXTLATTICE_MACOS_NOTARY_ISSUER_ID`

`CONTEXTLATTICE_MACOS_NOTARY_ISSUER_ID` is required for Team API keys and should
be omitted for Individual API keys.

Fallback notarization path: Apple ID app-specific password credentials:

- `CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID`
- `CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID`
- `CONTEXTLATTICE_MACOS_NOTARY_PASSWORD`

Local-only notarytool profile path:

- `CONTEXTLATTICE_MACOS_NOTARY_KEYCHAIN_PROFILE`

Use a keychain profile for local release runs. Prefer API key secrets for GitHub
Actions because hosted runners do not have your local Keychain profile.

The scripts also accept the older `PAID_MACOS_*` names for compatibility.

## Configure GitHub Secrets

Do not paste credentials into chat or commit them. Use the helper locally:

```bash
scripts/macos_configure_github_signing_secrets.sh \
  --cert-p12 ~/Downloads/DeveloperIDApplication.p12 \
  --notary-key-p8 ~/Downloads/AuthKey_ABC123DEFG.p8 \
  --notary-key-id ABC123DEFG \
  --notary-issuer-id 00000000-0000-0000-0000-000000000000 \
  --required-gates
```

For Apple ID app-specific password notarization instead of an API key:

```bash
scripts/macos_configure_github_signing_secrets.sh \
  --cert-p12 ~/Downloads/DeveloperIDApplication.p12 \
  --apple-id you@example.com \
  --team-id TEAMID1234 \
  --required-gates
```

The script prompts securely for the `.p12` password, the temporary CI keychain
password, and the Apple ID app-specific password when needed.

## Required Gates

By default, signing and notarization are optional so contributor and public
builds can still produce artifacts. To make CI fail when credentials are absent,
set repository or environment variables:

```bash
CONTEXTLATTICE_MACOS_SIGNING_REQUIRED=true
CONTEXTLATTICE_MACOS_NOTARIZATION_REQUIRED=true
```

For local ad-hoc signing tests, set:

```bash
CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY=-
CONTEXTLATTICE_MACOS_CODESIGN_TIMESTAMP=false
```

That validates bundle-signing mechanics without notarization.

## Verification

CI runs these checks when credentials are configured:

```bash
codesign --verify --deep --strict --verbose=2 "ContextLattice.app"
codesign --verify --deep --strict --verbose=2 "ContextLattice Monitoring.app"
codesign --verify --verbose dist/ContextLattice-macOS-universal.dmg
xcrun notarytool submit dist/ContextLattice-macOS-universal.dmg --wait ...
xcrun stapler staple dist/ContextLattice-macOS-universal.dmg
spctl --assess --type open --context context:primary-signature -v dist/ContextLattice-macOS-universal.dmg
```
