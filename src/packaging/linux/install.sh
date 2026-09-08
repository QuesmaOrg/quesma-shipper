#!/bin/sh
# Installs or upgrades quesma-shipper: TUF-capable bootstrap, optional login, background service.
# Re-running upgrades and keeps the login. Usage: install.sh [token --server URL] [--no-service]
set -eu

main() {
	BIN_DIR=$HOME/.local/bin
	STATE_DIR=${XDG_STATE_HOME:-$HOME/.local/state}/trajectory-shipper
	NAME=quesma-shipper
	TOKEN=${SHIPPER_AUTH_KEY:-} SERVER= NO_SERVICE= FROM=

	while [ $# -gt 0 ]; do
		case $1 in
		--server) SERVER=$2; shift 2 ;;
		--bin-dir) BIN_DIR=$2; shift 2 ;;
		--from) FROM=$2; shift 2 ;;
		--no-service) NO_SERVICE=1; shift ;;
		-h | --help) usage; exit 0 ;;
		--*) die "unknown option $1 (see --help)" ;;
		*) TOKEN=$1; shift ;;
		esac
	done

	[ "$(id -u)" != 0 ] || die "run this as yourself, not root: it installs into your home directory"
	[ -z "$FROM" ] || [ -f "$FROM" ] || die "--from $FROM: no such file"

	logged_in=
	[ -f "$STATE_DIR/enrollment.json" ] && logged_in=1
	[ -n "$logged_in" ] || [ -z "$TOKEN" ] || [ -n "$SERVER" ] || die "pass --server URL with the enrollment token"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	if [ -n "$FROM" ]; then
		cp "$FROM" "$tmp/$NAME"
	else
		download "$tmp/$NAME"
	fi

	mkdir -p "$BIN_DIR"
	chmod +x "$tmp/$NAME"
	rm -f "$BIN_DIR/$NAME"
	mv "$tmp/$NAME" "$BIN_DIR/$NAME"
	bin=$BIN_DIR/$NAME
	say "installed $("$bin" --version)"

	if [ -n "$logged_in" ]; then
		say "already logged in, keeping it"
	elif [ -n "$TOKEN" ]; then
		"$bin" login --server "$SERVER" "$TOKEN"
	else
		say "not enrolled; run \`$bin login <token> --server URL\`"
	fi
	[ -n "$NO_SERVICE" ] || "$bin" postinstall >/dev/null
	# A reinstall is the safe point to retire the former command: the service now points at the
	# new executable. Do not remove an unrelated program that happens to have the old name.
	legacy=$BIN_DIR/shipper
	if [ -x "$legacy" ] && "$legacy" --version 2>/dev/null | grep -q '^shipper '; then
		rm -f "$legacy"
	fi

	case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*) say "add $BIN_DIR to your PATH to run \`$NAME\` by name" ;;
	esac
}

download() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	arch=$(uname -m)
	case $arch in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $arch" ;;
	esac
	case $os in
	darwin) die "macOS releases are installed with Shipper.pkg; use --from for a local development binary" ;;
	linux) ;;
	*) die "unsupported OS $os" ;;
	esac
	# The bootstrap keeps its published target name; after launch its TUF client follows the
	# release manifest to quesma-shipper-* assets. Removing this target would strand old scripts.
	asset=shipper-$os-$arch
	case $os/$arch in
	linux/amd64) want=e15ae94795e1f8f00523a3874930b12494be0d568d67405e65abb59d323497ef ;;
	linux/arm64) want=70d4d4f9e96bd4356eb8fffcb76422e5925b757c1f56bd1c8254fe7f39ace4ea ;;
	esac
	base=https://updates.quesma.dev/targets

	say "downloading the TUF bootstrap for $os/$arch"
	curl -fsSL -o "$1" "$base/$want.$asset" || die "download of $asset failed"
	got=$(sha256sum "$1" 2>/dev/null || shasum -a 256 "$1")
	got=${got%% *}
	[ "$want" = "$got" ] || die "checksum mismatch for $asset: expected '$want', file is $got"
	say "checksum ok"
}

usage() {
	cat <<EOF
Installs or upgrades quesma-shipper into ~/.local/bin and turns on its background service.

  install.sh                    install or upgrade; an existing login is kept
  install.sh <token> --server URL
                                install and log in (supported for compatibility)
  install.sh --from PATH        install a local build instead of a release
  install.sh --no-service       skip the background service
  install.sh --bin-dir DIR      somewhere other than ~/.local/bin

Environment: SHIPPER_AUTH_KEY (the token, for headless installs).
EOF
}

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
say() { printf 'install: %s\n' "$*"; }

main "$@"
