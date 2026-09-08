#!/bin/sh
# Notarizes the signed macOS release, then staples the package ticket.
set -eu

main() {
	[ "$(uname -s)" = Darwin ] || die "macOS is required (notarytool, stapler)"
	[ $# -le 1 ] || usage

	OUT=${1:-bin/dist}
	PKG=$OUT/Shipper-macos-universal.pkg
	RAW_ARM64=$OUT/quesma-shipper-darwin-arm64
	RAW_AMD64=$OUT/quesma-shipper-darwin-amd64
	for artifact in "$PKG" "$RAW_ARM64" "$RAW_AMD64"; do
		[ -f "$artifact" ] || die "missing artifact: $artifact"
	done

	WORK=$(mktemp -d "${TMPDIR:-/tmp}/shipper-notary.XXXXXX")
	trap 'rm -rf "$WORK"' EXIT HUP INT TERM
	mkdir "$WORK/submission"
	cp "$PKG" "$RAW_ARM64" "$RAW_AMD64" "$WORK/submission"
	/usr/bin/ditto -c -k --keepParent "$WORK/submission" "$WORK/Shipper-notarization.zip"
	submit "$WORK/Shipper-notarization.zip"

	/usr/bin/xcrun stapler staple "$PKG"
	/usr/bin/xcrun stapler validate "$PKG"
	/usr/sbin/spctl --assess --type install --verbose=2 "$PKG"
}

submit() {
	if [ -n "${NOTARYTOOL_PROFILE:-}" ]; then
		/usr/bin/xcrun notarytool submit "$1" --keychain-profile "$NOTARYTOOL_PROFILE" --wait --timeout 20m
		return
	fi
	: "${APPLE_NOTARY_ID:?APPLE_NOTARY_ID is required}"
	: "${APPLE_NOTARY_TEAM_ID:?APPLE_NOTARY_TEAM_ID is required}"
	: "${APPLE_NOTARY_PASSWORD:?APPLE_NOTARY_PASSWORD is required}"
	/usr/bin/xcrun notarytool submit "$1" --apple-id "$APPLE_NOTARY_ID" \
		--team-id "$APPLE_NOTARY_TEAM_ID" --password "$APPLE_NOTARY_PASSWORD" --wait --timeout 20m
}

usage() {
	printf 'usage: %s [OUTPUT_DIR]\n' "$0" >&2
	exit 2
}

die() { printf 'notarize: %s\n' "$*" >&2; exit 1; }

main "$@"
