#!/usr/bin/env bash
set -euo pipefail

APK="apps/mobile/android/app/build/outputs/apk/release/app-release.apk"
ARTIFACT_DIR="artifact"
PACKAGE="cn.org.oxygent.handoff"

adb install -r "$APK"
adb shell monkey -p "$PACKAGE" 1

rm -f "$ARTIFACT_DIR/app-pid.txt"
for attempt in 1 2 3 4 5; do
  sleep 2
  if adb shell pidof "$PACKAGE" | tr -d '\r' > "$ARTIFACT_DIR/app-pid.txt" && test -s "$ARTIFACT_DIR/app-pid.txt"; then
    break
  fi
  echo "::warning::App process check $attempt failed; retrying"
done
test -s "$ARTIFACT_DIR/app-pid.txt"

screenshot_ok=0
for attempt in 1 2 3 4 5; do
  if adb wait-for-device && adb exec-out screencap -p > "$ARTIFACT_DIR/handoff-login.png.tmp" && test -s "$ARTIFACT_DIR/handoff-login.png.tmp"; then
    mv "$ARTIFACT_DIR/handoff-login.png.tmp" "$ARTIFACT_DIR/handoff-login.png"
    screenshot_ok=1
    break
  fi
  rm -f "$ARTIFACT_DIR/handoff-login.png.tmp"
  echo "::warning::Screenshot attempt $attempt failed; retrying"
  sleep 5
done
if [ "$screenshot_ok" -ne 1 ]; then
  echo "::warning::APK installation and app launch succeeded, but the emulator screenshot was unavailable"
fi

adb logcat -d > "$ARTIFACT_DIR/emulator-logcat.txt" || echo "::warning::Unable to collect emulator logcat"
