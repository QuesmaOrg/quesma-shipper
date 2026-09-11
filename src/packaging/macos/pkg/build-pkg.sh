#!/bin/sh
# Builds the universal Quesma Shipper.app and its rootless user-domain pkg.
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
	WORK=$(mktemp -d "${TMPDIR:-/tmp}/quesma-shipper-pkg.XXXXXX")
	trap 'rm -rf "$WORK"' EXIT HUP INT TERM
	APP="$WORK/payload/Applications/Quesma Shipper.app"
	mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" "$WORK/payload/.local/bin" "$OUT"

	build_binary "$MODULE" "$WORK" "$RELEASE_VERSION"
	cp "$WORK/quesma-shipper-arm64" "$OUT/quesma-shipper-darwin-arm64"
	cp "$WORK/quesma-shipper-amd64" "$OUT/quesma-shipper-darwin-amd64"
	/usr/bin/lipo -create "$WORK/quesma-shipper-arm64" "$WORK/quesma-shipper-amd64" \
		-output "$APP/Contents/MacOS/quesma-shipper"
	/usr/bin/lipo "$APP/Contents/MacOS/quesma-shipper" -verify_arch arm64 x86_64
	case $("$APP/Contents/MacOS/quesma-shipper" --version) in
	"quesma-shipper $RELEASE_VERSION ("*) ;;
	*) die "binary did not corroborate release version $RELEASE_VERSION (build from a clean matching revision)" ;;
	esac

	fill_bundle "$APP"
	ln -s '../../Applications/Quesma Shipper.app/Contents/MacOS/quesma-shipper' \
		"$WORK/payload/.local/bin/quesma-shipper"
	/usr/bin/xattr -cr "$WORK/payload" "$OUT/quesma-shipper-darwin-arm64" "$OUT/quesma-shipper-darwin-amd64"
	if [ -n "$APPLICATION_IDENTITY" ]; then
		sign_code "$APPLICATION_IDENTITY" "$APP" "$OUT/quesma-shipper-darwin-arm64" "$OUT/quesma-shipper-darwin-amd64"
	fi
	mkdir "$WORK/scripts"
	cp "$HERE/scripts/postinstall" "$WORK/scripts"

	build_component "$WORK/payload" com.quesma.shipper "$WORK/quesma-shipper-component.pkg" --scripts "$WORK/scripts"
	set -- --distribution "$HERE/Distribution.xml" --package-path "$WORK"
	[ -z "$INSTALLER_IDENTITY" ] || set -- "$@" --sign "$INSTALLER_IDENTITY"
	/usr/bin/productbuild "$@" "$OUT/quesma-shipper-macos-universal.pkg"
	if [ -n "$INSTALLER_IDENTITY" ]; then
		/usr/sbin/pkgutil --check-signature "$OUT/quesma-shipper-macos-universal.pkg" | grep -q 'Developer ID Installer:' || die "package is not Developer ID Installer signed"
	fi

	domains=$(/usr/sbin/installer -dominfo -pkg "$OUT/quesma-shipper-macos-universal.pkg")
	printf '%s\n' "$domains" | grep -q CurrentUserHomeDirectory || die "package does not enable the user home domain"
	if printf '%s\n' "$domains" | grep -q LocalSystem; then
		die "package unexpectedly enables the system domain"
	fi
	printf 'built %s\n' "$OUT/quesma-shipper-macos-universal.pkg"
}

# fill_bundle renders the versioned Info.plist and copies the resources the bundle carries.
fill_bundle() {
	bundle=$1
	sed -e "s/@MARKETING_VERSION@/$MARKETING_VERSION/g" \
		-e "s/@BUILD_VERSION@/$BUILD_VERSION/g" \
		-e "s/@RELEASE_VERSION@/$RELEASE_VERSION/g" \
		"$HERE/Info.plist.in" > "$bundle/Contents/Info.plist"
	/usr/bin/plutil -lint "$bundle/Contents/Info.plist" >/dev/null
	cp "$HERE/quesma-shipper.icns" "$bundle/Contents/Resources/quesma-shipper.icns"
	cp "$MODULE/internal/legal/LICENSE" "$MODULE/internal/legal/NOTICE" "$bundle/Contents/Resources/"
	cp -R "$MODULE/internal/legal/third_party" "$bundle/Contents/Resources/third_party"
}

# build_component packages root as one non-relocatable component; extra args go to pkgbuild.
build_component() {
	root=$1 identifier=$2 out=$3
	shift 3
	/usr/bin/pkgbuild --analyze --root "$root" "$out.components.plist"
	/usr/libexec/PlistBuddy -c 'Set :0:BundleIsRelocatable false' "$out.components.plist"
	/usr/bin/pkgbuild --root "$root" --component-plist "$out.components.plist" \
		--identifier "$identifier" --version "$BUILD_VERSION" --install-location / "$@" "$out"
}

sign_code() {
	identity=$1 app=$2 raw_arm64=$3 raw_amd64=$4
	for binary in "$raw_arm64" "$raw_amd64"; do
		/usr/bin/codesign --force --options runtime --timestamp \
			--identifier com.quesma.shipper.cli --sign "$identity" "$binary"
		/usr/bin/codesign --verify --strict --verbose=2 "$binary"
	done
	/usr/bin/codesign --force --options runtime --timestamp --sign "$identity" "$app"
	/usr/bin/codesign --verify --deep --strict --verbose=2 "$app"
}

build_binary() {
	module=$1 work=$2 version=$3
	ldflags="-X github.com/QuesmaOrg/quesma-shipper/internal/platform.releaseVersion=$version -s -w"
	for arch in arm64 amd64; do
		printf 'building quesma-shipper darwin/%s\n' "$arch"
		(cd "$module" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch \
			go build -trimpath -ldflags "$ldflags" -o "$work/quesma-shipper-$arch" ./cmd/quesma-shipper)
	done
}

usage() {
	printf 'usage: %s RELEASE_VERSION [OUTPUT_DIR]\n' "$0" >&2
	exit 2
}

die() { printf 'build-pkg: %s\n' "$*" >&2; exit 1; }

main "$@"
