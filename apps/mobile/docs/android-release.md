# Handoff Android release

## Current internal APK

- App: Handoff 0.1.0 (`versionCode` 1)
- Application ID: `cn.org.oxygent.handoff`
- Minimum Android: 7.0 / API 24
- Backend and web origin: `https://handoff.oxygent.org.cn`

The `Mobile Android Internal APK` GitHub Actions workflow creates an installable
APK artifact and records its SHA-256, byte size, package, version, and minimum
SDK. It builds the release variant so the JavaScript bundle is embedded, then
signs it with Android's debug key generated on the CI runner. This APK is for
standalone internal installation only; it is not a formally signed Play release.

From a clean checkout, the equivalent build is:

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm -C apps/mobile android:prod:prebuild
pnpm -C apps/mobile android:prod:internal
adb install -r apps/mobile/android/app/build/outputs/apk/release/app-release.apk
adb shell monkey -p cn.org.oxygent.handoff 1
```

The committed production env contains only public URLs. Never add a verification
code, signing key, service-account JSON, token, or password to it or to Expo
config. Staging's fixed verification code is not part of the production build.

## Static and emulator checks

Use `aapt dump badging` to verify package/version/minSdk and `apksigner verify
--print-certs` to identify the signer. Record `sha256sum` and `stat` for the exact
distributed file. On an API 24+ emulator, install and launch with the commands
above, capture the login screen, and inspect network traffic/logs to confirm
requests target `handoff.oxygent.org.cn` and never the staging host.

## Signed AAB and Google Play internal testing

Create a Google Play app for `cn.org.oxygent.handoff`, enable Play App Signing,
and keep the upload keystore and passwords in an approved secret manager. Add a
separate CI job that injects those secrets only at signing time and runs
`bundleRelease`; upload the resulting AAB to the Internal testing track with
release notes and named testers. Increment `versionCode` for every upload. The
internal APK and its debug key must never be promoted or reused as the Play
release signer. Production backend deployment is outside this mobile workflow.
