#!/bin/bash
# Real Installer/launchd lifecycle checks, restricted to a disposable macOS CI runner.
set -euo pipefail

[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_OS:-} == macOS ]] || {
  echo 'Run only on a disposable macOS GitHub Actions runner.' >&2
  exit 1
}
[[ $(id -u) != 0 ]] || { echo 'Run as the runner user, with passwordless sudo.' >&2; exit 1; }

pkg="$PWD/bin/dist/quesma-shipper-macos-universal.pkg"
remover="$PWD/src/packaging/macos/mdm/uninstall.sh"
label=com.quesma.shipper
target="gui/$(id -u)/$label"
personal_app="$HOME/Applications/Quesma Shipper.app"
system_app='/Applications/Quesma Shipper.app'
personal_plist="$HOME/Library/LaunchAgents/$label.plist"
system_plist="/Library/LaunchAgents/$label.plist"
personal_exe="$personal_app/Contents/MacOS/quesma-shipper"
personal_link="$HOME/.local/bin/quesma-shipper"
system_link=/usr/local/bin/quesma-shipper
state="$HOME/.local/state/trajectory-shipper"

for path in "$personal_app" "$system_app" "$personal_plist" "$system_plist" "$personal_link" "$system_link" "$state"; do
  [[ ! -e "$path" && ! -L "$path" ]] || { echo "Existing installation at $path; refusing lifecycle test." >&2; exit 1; }
done
if launchctl print "$target" >/dev/null 2>&1; then
  echo 'An agent is already loaded; refusing lifecycle test.' >&2
  exit 1
fi

work=$(mktemp -d)
cleanup() {
  launchctl bootout "$target" >/dev/null 2>&1 || true
  sudo /bin/sh "$remover" >/dev/null 2>&1 || true
  rm -rf "$personal_app" "$personal_plist" "$personal_link" "$state" "$work"
}
trap cleanup EXIT
mkdir -p "$state"
printf 'synthetic enrollment marker\n' > "$state/issue59-enrollment"
printf 'synthetic upload history marker\n' > "$state/issue59-upload-history"

assert_state() {
  [[ $(cat "$state/issue59-enrollment") == 'synthetic enrollment marker' ]]
  [[ $(cat "$state/issue59-upload-history") == 'synthetic upload history marker' ]]
}
assert_loaded() {
  local path
  path=$(launchctl print "$target" | sed -n $'s/^\tpath = //p')
  [[ "$path" == "$1" ]] || { echo "Expected loaded plist $1, got $path" >&2; exit 1; }
}
assert_gone() {
  local attempt
  for ((attempt=0; attempt<30; attempt++)); do
    if ! launchctl print "$target" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  if launchctl print "$target" >/dev/null 2>&1; then
    echo 'Agent still loaded after uninstall.' >&2
    exit 1
  fi
}
assert_system_removed() {
  [[ ! -e "$system_app" && ! -e "$system_plist" && ! -L "$system_link" ]]
  if pkgutil --pkg-info "$label" >/dev/null 2>&1; then
    echo 'System receipt survived removal.' >&2
    exit 1
  fi
  assert_state
}
install_personal() {
  installer -pkg "$pkg" -target CurrentUserHomeDirectory
  assert_loaded "$personal_plist"
  [[ $(readlink "$personal_link") == "$personal_exe" ]]
  assert_state
}
install_system() {
  sudo installer -pkg "$pkg" -target /
  assert_loaded "$system_plist"
  [[ $(readlink "$system_link") == "$system_app/Contents/MacOS/quesma-shipper" ]]
  assert_state
}
remove_personal() {
  "$personal_exe" uninstall --yes
  assert_gone
  [[ ! -e "$personal_app" && ! -e "$personal_plist" && ! -L "$personal_link" ]]
  assert_state
}
restore_personal() {
  ditto "$work/personal.app" "$personal_app"
  mkdir -p "$(dirname "$personal_plist")" "$(dirname "$personal_link")"
  cp "$work/personal.plist" "$personal_plist"
  ln -s "$personal_exe" "$personal_link"
}
assert_personal_preserved() {
  diff -qr "$work/personal.app" "$personal_app"
  cmp "$work/personal.plist" "$personal_plist"
  [[ $(readlink "$personal_link") == "$personal_exe" ]]
  assert_state
}

echo 'Root inspection distinguishes missing GUI domains and missing services'
exit_code=0
sudo launchctl print "gui/987654/$label" > "$work/absent-domain.txt" 2>&1 || exit_code=$?
[[ $exit_code == 112 ]] || { echo "Missing GUI domain returned $exit_code, expected 112" >&2; exit 1; }
exit_code=0
sudo launchctl print "$target" > "$work/absent-service.txt" 2>&1 || exit_code=$?
[[ $exit_code == 113 ]] || { echo "Missing service returned $exit_code, expected 113" >&2; exit 1; }

echo 'Personal install, repair, conflict rejection, removal, and reinstall'
install_personal
install_personal
ditto "$personal_app" "$work/personal.app"
cp "$personal_plist" "$work/personal.plist"
if sudo installer -pkg "$pkg" -target /; then
  echo 'System install accepted an existing personal installation.' >&2; exit 1
fi
assert_loaded "$personal_plist"
assert_personal_preserved
remove_personal
install_personal
remove_personal

echo 'System install, repair, conflict rejection, and idempotent removal'
install_system
install_system
if "$system_link" uninstall --yes; then
  echo 'Personal uninstall accepted the system executable.' >&2; exit 1
fi
assert_loaded "$system_plist"
if installer -pkg "$pkg" -target CurrentUserHomeDirectory; then
  echo 'Personal install accepted an existing system installation.' >&2; exit 1
fi
assert_loaded "$system_plist"
sudo /bin/sh "$remover"
assert_gone
assert_system_removed
sudo /bin/sh "$remover"
assert_system_removed

echo 'Remove a loaded system agent while preserving a dormant personal installation'
install_system
restore_personal
sudo /bin/sh "$remover"
assert_gone
assert_system_removed
assert_personal_preserved
"$personal_exe" postinstall
assert_loaded "$personal_plist"
remove_personal

echo 'Remove system files while preserving the loaded personal agent'
install_system
restore_personal
launchctl bootout "$target"
assert_gone
launchctl bootstrap "gui/$(id -u)" "$personal_plist"
assert_loaded "$personal_plist"
sudo /bin/sh "$remover" | tee "$work/mixed-removal.txt"
grep -qF "Leaving non-system agent for uid $(id -u) loaded." "$work/mixed-removal.txt"
grep -qF 'system installation removed; personal installations, if any, were left intact' "$work/mixed-removal.txt"
assert_system_removed
assert_personal_preserved
assert_loaded "$personal_plist"
remove_personal

echo 'Removal preserves a submitted job without a source plist'
install_system
launchctl bootout "$target"
assert_gone
launchctl submit -l "$label" -- /bin/sleep 600
submitted_pid=''
for ((attempt=0; attempt<30; attempt++)); do
  sudo launchctl print "$target" > "$work/submitted-before.txt"
  submitted_pid=$(sed -n $'s/^\tpid = //p' "$work/submitted-before.txt")
  [[ -z "$submitted_pid" ]] || break
  sleep 1
done
[[ -n "$submitted_pid" ]]
if grep -q $'^\tpath = ' "$work/submitted-before.txt"; then
  echo 'Submitted job unexpectedly has a source plist.' >&2; exit 1
fi
grep -q $'^\tprogram = /bin/sleep$' "$work/submitted-before.txt"
sudo /bin/sh "$remover" | tee "$work/submitted-removal.txt"
assert_system_removed
grep -qF "Leaving non-system agent for uid $(id -u) loaded." "$work/submitted-removal.txt"
sudo launchctl print "$target" > "$work/submitted-after.txt"
[[ $(sed -n $'s/^\tpid = //p' "$work/submitted-after.txt") == "$submitted_pid" ]]
launchctl bootout "$target"
assert_gone

echo 'Reinstall after recovery, then personal purge'
install_personal
"$personal_exe" uninstall --purge --yes
assert_gone
[[ ! -e "$personal_app" && ! -e "$personal_plist" && ! -L "$personal_link" && ! -e "$state" ]]
echo 'macOS personal/system package lifecycle passed'
