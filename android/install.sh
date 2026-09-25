#!/usr/bin/env bash
# Builds the debug APK and installs it on a connected device over adb.
#
# Env: ANDROID_SDK_ROOT (a Linux-native SDK for the build - see
# CLAUDE.md), ADB (the adb that can reach the device; defaults to the
# Windows-side one, since WSL can't see a phone paired over Wi-Fi/USB on
# the host), ADB_SERIAL (the device; defaults to the only one connected).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

export ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-$HOME/android-sdk-toolchains}"
ADB="${ADB:-/mnt/c/Users/Rhino/AppData/Local/Android/Sdk/platform-tools/adb.exe}"

if [ -z "${ADB_SERIAL:-}" ]; then
  mapfile -t devices < <("$ADB" devices | tr -d '\r' | awk 'NR > 1 && $2 == "device" { print $1 }')
  if [ "${#devices[@]}" -ne 1 ]; then
    echo "expected exactly one connected device, found ${#devices[@]} - set ADB_SERIAL:" >&2
    "$ADB" devices >&2
    exit 1
  fi
  ADB_SERIAL="${devices[0]}"
fi

./gradlew -q assembleDebug
apk=app/build/outputs/apk/debug/app-debug.apk
# A Windows adb.exe needs a Windows path to the APK.
if [[ "$ADB" == *.exe ]]; then apk="$(wslpath -w "$apk")"; fi
"$ADB" -s "$ADB_SERIAL" install -r "$apk"
