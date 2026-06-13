# macOS Signing And Notarization

ContextLattice can build the macOS launcher DMG without Apple credentials. When
Developer ID credentials are configured, the same release lane signs the app
bundles before DMG creation, signs the final DMG, submits the DMG for
notarization, staples the ticket, and runs a Gatekeeper assessment.

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

## GitHub Secrets

Prefer the neutral secret names below for public and paid release lanes:

- `CONTEXTLATTICE_MACOS_CERT_P12_BASE64`
- `CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD`
- `CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD`
- `CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY`
- `CONTEXTLATTICE_MACOS_NOTARY_KEYCHAIN_PROFILE`

If a notary keychain profile is not used, configure Apple ID credentials:

- `CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID`
- `CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID`
- `CONTEXTLATTICE_MACOS_NOTARY_PASSWORD`

The scripts also accept the older `PAID_MACOS_*` names for compatibility.

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
