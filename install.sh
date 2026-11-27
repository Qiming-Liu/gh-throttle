#!/bin/sh
# Install or update GitHub CLI Throttle on macOS or Linux:
#
#   curl -fsSL https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.sh | sh
#
# Run the same command again to update. If the official GitHub CLI is
# installed, it offers to uninstall it, and only does so after you answer y.
#
# Environment:
#   GH_THROTTLE_VERSION=2.106.0     install this release instead of the latest
#   GH_THROTTLE_BIN_DIR=DIR         where gh goes (default ~/.local/bin)
#   GH_THROTTLE_NO_MODIFY_PATH=1    leave shell profiles alone
set -eu

REPO="Qiming-Liu/gh-throttle"
BIN_DIR="${GH_THROTTLE_BIN_DIR:-$HOME/.local/bin}"
SHARE_DIR="$HOME/.local/share/gh-throttle"

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
has() { command -v "$1" >/dev/null 2>&1; }

# ask QUESTION: true only when the user types y or Y. Reads the terminal,
# since stdin is this script when it is piped into sh.
ask() {
	# A subshell, because a failed redirection on ':' exits a POSIX shell.
	if ! (: </dev/tty) 2>/dev/null; then
		return 1
	fi
	printf '%s [y/N] ' "$1" >/dev/tty
	read -r answer </dev/tty || return 1
	[ "$answer" = y ] || [ "$answer" = Y ]
}

fetch() { # fetch URL FILE
	if has curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	else
		wget -q -O "$2" "$1"
	fi
}

latest_tag() {
	url="https://github.com/$REPO/releases/latest"
	if has curl; then
		final=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$url")
	else
		final=$(wget -S --spider "$url" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r')
	fi
	tag=${final##*/}
	case "$tag" in
		v*-throttle) printf '%s\n' "$tag" ;;
		*) die "could not find the latest release at $url" ;;
	esac
}

sha256() {
	if has sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif has shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "need sha256sum or shasum to verify the download"
	fi
}

if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi

# uninstall_cmd prints the command that removes the official gh, if a known
# package manager installed it.
uninstall_cmd() {
	if has brew && brew list --formula gh >/dev/null 2>&1; then
		echo "brew uninstall gh"
	elif has dpkg-query && [ "$(dpkg-query -W -f='${Status}' gh 2>/dev/null)" = "install ok installed" ]; then
		echo "$SUDO apt-get remove -y gh"
	elif has rpm && rpm -q gh >/dev/null 2>&1; then
		if has dnf; then echo "$SUDO dnf remove -y gh"
		elif has yum; then echo "$SUDO yum remove -y gh"
		elif has zypper; then echo "$SUDO zypper --non-interactive remove gh"
		else echo "$SUDO rpm -e gh"
		fi
	elif has pacman && pacman -Q github-cli >/dev/null 2>&1; then
		echo "$SUDO pacman -R --noconfirm github-cli"
	elif has apk && apk info -e github-cli >/dev/null 2>&1; then
		echo "$SUDO apk del github-cli"
	elif has snap && snap list gh >/dev/null 2>&1; then
		echo "$SUDO snap remove gh"
	elif has port && port installed gh 2>/dev/null | grep -q active; then
		echo "$SUDO port uninstall gh"
	fi
}

# --- platform ---
case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU $(uname -m)" ;;
esac
has curl || has wget || die "need curl or wget"
has tar || die "need tar"

# --- release ---
if [ -n "${GH_THROTTLE_VERSION:-}" ]; then
	tag="v${GH_THROTTLE_VERSION#v}"
	tag="${tag%-throttle}-throttle"
else
	tag=$(latest_tag)
fi
version=${tag#v}
name="gh_${version}_${os}_${arch}"
say "Installing GitHub CLI Throttle $version ($os/$arch)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
base="https://github.com/$REPO/releases/download/$tag"
fetch "$base/$name.tar.gz" "$tmp/$name.tar.gz" || die "download failed: $base/$name.tar.gz"
fetch "$base/gh_${version}_checksums.txt" "$tmp/checksums.txt" || die "download failed: checksums"
want=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$name.tar.gz is not in the checksums file"
[ "$(sha256 "$tmp/$name.tar.gz")" = "$want" ] || die "checksum mismatch for $name.tar.gz"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

# --- other gh on PATH ---
target="$BIN_DIR/gh"
seen=""
old_ifs=$IFS
IFS=:
for dir in $PATH; do
	IFS=$old_ifs
	g="$dir/gh"
	[ -x "$g" ] && [ ! -d "$g" ] || continue
	[ "$g" = "$target" ] && continue
	case " $seen " in *" $g "*) continue ;; esac
	seen="$seen $g"
	if "$g" throttle --help >/dev/null 2>&1; then
		warn "another copy of GitHub CLI Throttle is at $g; remove it if you don't need it"
		continue
	fi
	say "Found the official GitHub CLI at $g"
	cmd=$(uninstall_cmd)
	if [ -z "$cmd" ]; then
		warn "can't tell how $g was installed; please uninstall it yourself"
	elif ask "Uninstall it with '$cmd'?"; then
		sh -c "$cmd" || warn "uninstall failed; please remove $g yourself"
	else
		warn "keeping $g; make sure $BIN_DIR comes before it on PATH"
	fi
done
IFS=$old_ifs

# --- install ---
mkdir -p "$BIN_DIR" "$SHARE_DIR"
cp "$tmp/$name/bin/gh" "$tmp/gh.new"
chmod 755 "$tmp/gh.new"
mv -f "$tmp/gh.new" "$target"
cp "$tmp/$name/LICENSE" "$tmp/$name/LICENSE-ghx" "$SHARE_DIR/"
"$target" --version >/dev/null || die "$target does not run"
say "Installed $target"

# --- PATH ---
case ":$PATH:" in
	*":$BIN_DIR:"*) on_path=1 ;;
	*) on_path=0 ;;
esac
if [ "$on_path" = 0 ] && [ "${GH_THROTTLE_NO_MODIFY_PATH:-}" != 1 ]; then
	marker="# gh-throttle"
	case "${SHELL:-}" in
		*/zsh) profile="$HOME/.zshrc" ;;
		*/bash) if [ "$os" = darwin ]; then profile="$HOME/.bash_profile"; else profile="$HOME/.bashrc"; fi ;;
		*/fish) profile="$HOME/.config/fish/conf.d/gh-throttle.fish" ;;
		*) profile="$HOME/.profile" ;;
	esac
	if ! grep -qs "$marker" "$profile"; then
		mkdir -p "$(dirname "$profile")"
		case "$profile" in
			*.fish) printf '\n%s\nfish_add_path --move --path "%s"\n' "$marker" "$BIN_DIR" >>"$profile" ;;
			*) printf '\n%s\nexport PATH="%s:$PATH"\n' "$marker" "$BIN_DIR" >>"$profile" ;;
		esac
		say "Added $BIN_DIR to PATH in $profile"
	fi
	say "Open a new terminal, or run: export PATH=\"$BIN_DIR:\$PATH\""
elif [ "$on_path" = 0 ]; then
	warn "$BIN_DIR is not on PATH"
fi

say ""
say "Done. Next:"
say "  gh auth login      # if you haven't signed in yet"
say "  gh throttle status"
