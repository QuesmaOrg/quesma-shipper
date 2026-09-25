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

users=$(/usr/bin/dscl . -list /Users UniqueID)
console_uid=$(/usr/bin/stat -f %u /dev/console)
if [ "$console_uid" -ge 500 ]; then
	console_name=$(/usr/bin/stat -f %Su /dev/console)
	users=$(printf '%s\n%s %s\n' "$users" "$console_name" "$console_uid")
fi
printf '%s\n' "$users" | while read -r name uid; do
	[ "$uid" -ge 500 ] || continue
	target="gui/$uid/$label"
	if /bin/launchctl print "$target" >/dev/null 2>&1; then
		home=$(/usr/bin/dscl /Search -read "/Users/$name" NFSHomeDirectory)
		home=${home#NFSHomeDirectory: }
		if [ -e "$home/Library/LaunchAgents/$label.plist" ] || [ -L "$home/Library/LaunchAgents/$label.plist" ]; then
			printf 'A per-user agent exists for %s; resolve the mixed installation before removal.\n' "$name" >&2
			exit 1
		fi
		/bin/launchctl bootout "$target"
		remaining=360
		while /bin/launchctl print "$target" >/dev/null 2>&1; do
			[ "$remaining" -gt 0 ] || { printf 'Agent %s is still loaded; files left in place.\n' "$target" >&2; exit 1; }
			sleep 1
			remaining=$((remaining - 1))
		done
	fi
done

rm -f "$plist"
if [ -L /usr/local/bin/quesma-shipper ] && [ "$(readlink /usr/local/bin/quesma-shipper)" = "$exe" ]; then
	rm /usr/local/bin/quesma-shipper
fi
rm -rf "$app"
if /usr/sbin/pkgutil --pkg-info "$label" >/dev/null 2>&1; then
	/usr/sbin/pkgutil --forget "$label"
fi
printf 'Quesma Shipper removed; per-user enrollment and upload history kept. Remove its enrollment profile in MDM as well.\n'
