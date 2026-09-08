#!/bin/sh
# Builds the monotonic release identity from the reviewed release line and Git provenance.
#
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
base=$(tr -d '\r\n' < "$repo/VERSION")

if ! printf '%s\n' "$base" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
	printf 'VERSION must contain stable SemVer (MAJOR.MINOR.PATCH), got %s\n' "$base" >&2
	exit 1
fi

count=$(git -C "$repo" rev-list --count HEAD)
sha=$(git -C "$repo" rev-parse --short=12 HEAD)
printf '%s-%s.%s\n' "$base" "$count" "$sha"
