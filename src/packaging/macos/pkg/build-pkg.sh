#!/bin/sh
# Builds the universal Shipper.app and its rootless user-domain pkg.
set -eu

main() {
	[ "$(uname -s)" = Darwin ] || die "macOS is required (pkgbuild, productbuild, lipo)"
	[ $# -ge 1 ] && [ $# -le 2 ] || usage

	RELEASE_VERSION=$1
	OUT=${2:-bin/dist}
	APPLICATION_IDENTITY=${MACOS_APPLICATION_IDENTITY:-}
	INSTALLER_IDENTITY=${MACOS_INSTALLER_IDENTITY:-}
	if [ -n "$APPLICATION_IDENTITY" ] || [ -n "$INSTALLER_IDENTITY" ]; then
		[ -n "$APPLICATION_IDENTITY" ] && [ -n "$INSTALLER_IDENTITY" ] || die "both macOS signing identities are required"
	fi
	MARKETING_VERSION=${RELEASE_VERSION%%[-+]*}
	case $RELEASE_VERSION in
	*-*) BUILD_VERSION=${RELEASE_VERSION#*-}; BUILD_VERSION=${BUILD_VERSION%%.*} ;;
	*) BUILD_VERSION=$MARKETING_VERSION ;;
	esac
	case $RELEASE_VERSION in *[!0-9A-Za-z.+-]*) die "invalid release version: $RELEASE_VERSION" ;; esac
	case $MARKETING_VERSION in [0-9]*.[0-9]*.[0-9]*) ;; *) die "version must start with three integers: $RELEASE_VERSION" ;; esac
	case $BUILD_VERSION in ''|*[!0-9.]*) die "build version must contain only integers and dots: $BUILD_VERSION" ;; esac

	HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
	MODULE=$(CDPATH= cd -- "$HERE/../../.." && pwd)
	WORK=$(mktemp -d "${TMPDIR:-/tmp}/shipper-pkg.XXXXXX")
	trap 'rm -rf "$WORK"' EXIT HUP INT TERM
	APP=$WORK/payload/Applications/Shipper.app
	mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" "$WORK/payload/.local/bin" "$OUT"

	build_binary "$MODULE" "$WORK" "$RELEASE_VERSION"
	cp "$WORK/shipper-arm64" "$OUT/quesma-shipper-darwin-arm64"
	cp "$WORK/shipper-amd64" "$OUT/quesma-shipper-darwin-amd64"
	/usr/bin/lipo -create "$WORK/shipper-arm64" "$WORK/shipper-amd64" -output "$APP/Contents/MacOS/shipper"
	/usr/bin/lipo "$APP/Contents/MacOS/shipper" -verify_arch arm64 x86_64
	case $("$APP/Contents/MacOS/shipper" --version) in
	"quesma-shipper $RELEASE_VERSION ("*) ;;
	*) die "binary did not corroborate release version $RELEASE_VERSION (build from a clean matching revision)" ;;
	esac

	sed -e "s/@MARKETING_VERSION@/$MARKETING_VERSION/g" \
		-e "s/@BUILD_VERSION@/$BUILD_VERSION/g" \
		-e "s/@RELEASE_VERSION@/$RELEASE_VERSION/g" \
		"$HERE/Info.plist.in" > "$APP/Contents/Info.plist"
	/usr/bin/plutil -lint "$APP/Contents/Info.plist" >/dev/null
	cp "$HERE/Shipper.icns" "$APP/Contents/Resources/Shipper.icns"
	cp "$MODULE/internal/legal/LICENSE" "$MODULE/internal/legal/NOTICE" "$APP/Contents/Resources/"
	cp -R "$MODULE/internal/legal/third_party" "$APP/Contents/Resources/third_party"
	ln -s ../../Applications/Shipper.app/Contents/MacOS/shipper "$WORK/payload/.local/bin/quesma-shipper"
	/usr/bin/xattr -cr "$WORK/payload" "$OUT/quesma-shipper-darwin-arm64" "$OUT/quesma-shipper-darwin-amd64"
	if [ -n "$APPLICATION_IDENTITY" ]; then
		sign_code "$APPLICATION_IDENTITY" "$APP" "$OUT/quesma-shipper-darwin-arm64" "$OUT/quesma-shipper-darwin-amd64"
	fi
	mkdir "$WORK/scripts"
	cp "$HERE/scripts/postinstall" "$WORK/scripts"

	/usr/bin/pkgbuild --analyze --root "$WORK/payload" "$WORK/components.plist"
	/usr/libexec/PlistBuddy -c 'Set :0:BundleIsRelocatable false' "$WORK/components.plist"
	/usr/bin/pkgbuild --root "$WORK/payload" \
		--component-plist "$WORK/components.plist" \
		--scripts "$WORK/scripts" \
		--identifier com.quesma.trajectory-shipper \
		--version "$BUILD_VERSION" --install-location / \
		"$WORK/Shipper-component.pkg"
	set -- --distribution "$HERE/Distribution.xml" --package-path "$WORK"
	[ -z "$INSTALLER_IDENTITY" ] || set -- "$@" --sign "$INSTALLER_IDENTITY"
	/usr/bin/productbuild "$@" "$OUT/Shipper-macos-universal.pkg"
	if [ -n "$INSTALLER_IDENTITY" ]; then
		/usr/sbin/pkgutil --check-signature "$OUT/Shipper-macos-universal.pkg" | grep -q 'Developer ID Installer:' || die "package is not Developer ID Installer signed"
	fi

	domains=$(/usr/sbin/installer -dominfo -pkg "$OUT/Shipper-macos-universal.pkg")
	printf '%s\n' "$domains" | grep -q CurrentUserHomeDirectory || die "package does not enable the user home domain"
	if printf '%s\n' "$domains" | grep -q LocalSystem; then
		die "package unexpectedly enables the system domain"
	fi
	printf 'built %s\n' "$OUT/Shipper-macos-universal.pkg"
}

sign_code() {
	identity=$1 app=$2 raw_arm64=$3 raw_amd64=$4
	for binary in "$raw_arm64" "$raw_amd64"; do
		/usr/bin/codesign --force --options runtime --timestamp \
			--identifier com.quesma.trajectory-shipper.cli --sign "$identity" "$binary"
		/usr/bin/codesign --verify --strict --verbose=2 "$binary"
	done
	/usr/bin/codesign --force --options runtime --timestamp --sign "$identity" "$app"
	/usr/bin/codesign --verify --deep --strict --verbose=2 "$app"
}

build_binary() {
	module=$1 work=$2 version=$3
	ldflags="-X github.com/QuesmaOrg/quesma-shipper/internal/platform.releaseVersion=$version -s -w"
	for arch in arm64 amd64; do
		printf 'building shipper darwin/%s\n' "$arch"
		(cd "$module" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch \
			go build -trimpath -ldflags "$ldflags" -o "$work/shipper-$arch" ./cmd/quesma-shipper)
	done
}

usage() {
	printf 'usage: %s RELEASE_VERSION [OUTPUT_DIR]\n' "$0" >&2
	exit 2
}

die() { printf 'build-pkg: %s\n' "$*" >&2; exit 1; }

main "$@"
