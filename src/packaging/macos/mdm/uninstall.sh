#!/bin/sh
# Administrator removal keeps each user's enrollment and upload history for reinstall.
set -eu

[ "$(id -u)" = 0 ] || { printf 'Run this script as an administrator (root).\n' >&2; exit 1; }

label=com.quesma.shipper
app='/Applications/Quesma Shipper.app'
plist="/Library/LaunchAgents/$label.plist"
exe="$app/Contents/MacOS/quesma-shipper"

if [ ! -e "$app" ] && [ ! -L "$app" ] && [ ! -e "$plist" ] && [ ! -L "$plist" ]; then
	printf 'No system installation found; nothing removed.\n'
	exit 0
fi
[ ! -L "$app" ] && [ ! -L "$plist" ] || { printf 'Refusing to remove a symlinked system installation.\n' >&2; exit 1; }

if [ -e "$plist" ]; then
	program=$(/usr/bin/plutil -extract ProgramArguments.0 raw -o - "$plist")
	[ "$program" = "$exe" ] || { printf 'Refusing to remove an unrecognized LaunchAgent.\n' >&2; exit 1; }
fi
if [ -e "$app" ]; then
	identifier=$(/usr/bin/plutil -extract CFBundleIdentifier raw -o - "$app/Contents/Info.plist")
	[ "$identifier" = "$label" ] || { printf 'Refusing to remove an unrecognized app.\n' >&2; exit 1; }
fi

if [ -e "$app" ]; then
	[ -f "$exe" ] && [ ! -L "$exe" ] && [ "$(/usr/bin/stat -f %u "$exe")" = 0 ] || {
		printf 'Refusing to run an untrusted system executable.\n' >&2
		exit 1
	}
	"$exe" preuninstall-system
elif [ -e "$plist" ]; then
	printf 'LaunchAgent remains without the app; restore the package before removal.\n' >&2
	exit 1
fi

rm -f "$plist"
if [ -L /usr/local/bin/quesma-shipper ] && [ "$(readlink /usr/local/bin/quesma-shipper)" = "$exe" ]; then
	rm /usr/local/bin/quesma-shipper
fi
rm -rf "$app"
if /usr/sbin/pkgutil --pkg-info "$label" >/dev/null 2>&1; then
	/usr/sbin/pkgutil --forget "$label"
fi
printf 'Quesma Shipper removed; per-user enrollment and upload history kept. Remove its enrollment profile in MDM as well.\n'
