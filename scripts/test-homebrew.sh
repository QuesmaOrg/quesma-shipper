#!/bin/bash
# Exercises the real Homebrew and launchd lifecycle on a disposable macOS CI runner.
set -euo pipefail

[[ ${CI:-} == true && $(uname -s) == Darwin ]] || { echo 'Run only on a disposable macOS CI runner' >&2; exit 1; }
plist="$HOME/Library/LaunchAgents/com.quesma.shipper.plist"
[[ ! -e "$plist" ]] || { echo 'An existing shipper service must not be touched' >&2; exit 1; }
[[ ! -e "$(brew --caskroom)/quesma-shipper" ]] || { echo 'An existing cask must not be touched' >&2; exit 1; }

export HOMEBREW_NO_AUTO_UPDATE=1
tap="$(brew --repository)/Library/Taps/quesma-test/homebrew-shipper"
[[ ! -e "$tap" ]] || { echo 'Test tap already exists' >&2; exit 1; }
mkdir -p "$tap/Casks"
work=$(mktemp -d)
cleanup() {
  if brew list --cask quesma-test/shipper/quesma-shipper >/dev/null 2>&1; then
    brew uninstall --cask quesma-test/shipper/quesma-shipper || true
  fi
  rm -rf "$tap" "$work"
}
trap cleanup EXIT

write_cask() {
  python3 scripts/homebrew-cask.py "$1" bin/dist > "$work/quesma-shipper.rb"
  python3 - "$work/quesma-shipper.rb" "$tap/Casks/quesma-shipper.rb" "$PWD/bin/dist" <<'PY'
from pathlib import Path
import sys
source, target, artifacts = map(Path, sys.argv[1:])
text = source.read_text().replace(
    'https://updates.quesma.dev/targets/#{sha256}.quesma-shipper-darwin-#{arch}',
    artifacts.as_uri() + '/quesma-shipper-darwin-#{arch}',
)
# Only the locally built CI fixture lacks production signing and notarization.
text = text.replace('  uninstall script:', '''  preflight_steps do
    run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{staged_path}}/quesma-shipper"]
  end

  uninstall script:''')
target.write_text(text)
PY
}

check_service() {
  for attempt in {1..10}; do
    launchctl print "gui/$(id -u)/com.quesma.shipper" > "$work/service.txt"
    if grep -q 'pid = ' "$work/service.txt"; then break; fi
    sleep 1
  done
  grep -q 'pid = ' "$work/service.txt"
  grep -q "/$1/quesma-shipper" "$plist"
}

write_cask 1.0.0
brew install --cask --yes quesma-test/shipper/quesma-shipper
quesma-shipper --version
check_service 1.0.0
state="$HOME/.local/state/trajectory-shipper"
mkdir -p "$state"
printf 'preserve\n' > "$state/brew-smoke-marker"
if quesma-shipper uninstall > "$work/uninstall.txt" 2>&1; then exit 1; fi
grep -q 'brew uninstall' "$work/uninstall.txt"

write_cask 1.0.1
brew upgrade --cask --greedy --yes quesma-test/shipper/quesma-shipper
check_service 1.0.1
brew uninstall --cask quesma-test/shipper/quesma-shipper
[[ ! -e "$plist" ]]
if launchctl print "gui/$(id -u)/com.quesma.shipper" >/dev/null 2>&1; then exit 1; fi
grep -qx preserve "$state/brew-smoke-marker"
rm "$state/brew-smoke-marker"
