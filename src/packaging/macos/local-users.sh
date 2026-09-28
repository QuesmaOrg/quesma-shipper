#!/bin/sh
set -eu

uids=$(/usr/bin/dscl . -list /Users UniqueID)
homes=$(/usr/bin/dscl . -list /Users NFSHomeDirectory)
printf '%s\n--homes--\n%s\n' "$uids" "$homes" | /usr/bin/awk '
  $0 == "--homes--" { reading_homes = 1; next }
  !reading_homes { uid[$1] = $2; next }
  {
    name = $1
    sub(/^[^[:space:]]+[[:space:]]+/, "", $0)
    home[name] = $0
  }
  END {
    for (name in uid) {
      if (uid[name] + 0 >= 500)
        printf "%s\t%s\n", uid[name], home[name]
    }
  }
'

console_uid=$(/usr/bin/stat -f %u /dev/console)
if [ "$console_uid" -ge 500 ]; then
	console_name=$(/usr/bin/stat -f %Su /dev/console)
	console_home=$(/usr/bin/dscl /Search -read "/Users/$console_name" NFSHomeDirectory)
	console_home=${console_home#NFSHomeDirectory:}
	console_home=$(printf '%s\n' "$console_home" | /usr/bin/awk 'NF { sub(/^[[:space:]]+/, ""); print; exit }')
	printf '%s\t%s\n' "$console_uid" "$console_home"
fi
