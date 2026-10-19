#!/bin/sh
# Builds bin/brain. herdr runs this on `herdr plugin install`; run it yourself before `herdr plugin link .`.
#
# Release binary first: when the checkout is clean and HEAD is the commit of tag
# v<manifest version>, it installs the release asset, also with Go on PATH. The asset
# must match checksums.txt, and its `version` must print the manifest version.
# If the asset cannot be used, it prints one line on stderr and builds from source with Go.
# Any other ref builds this checkout from source, so a source change always reaches bin/brain.
# With no Go, such a ref stops with "this ref needs Go".
# The asset path needs a public repo: a private repo cannot serve release assets without auth.
# After a build of `herdr plugin install`, it starts the `start` action (see auto_start).
set -eu
cd "$(dirname "$0")/.."

version=$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml)
tag="v${version}"
mkdir -p bin

has_go=no
if command -v go >/dev/null 2>&1; then
	has_go=yes
fi

die() {
	echo "brain build: $*" >&2
	exit 1
}

# auto_start runs at the end of a good build (E19-S3, #427): herdr runs [[startup]] only after
# a session restore, so after an install nothing would start brained or setup. The new
# bin/brain post-install writes a random nonce to bin/.install-nonce. Then a detached child of
# it waits up to 60 s for `herdr plugin list --json` to list a GitHub install of this HEAD whose
# plugin root holds that nonce (when herdr listed this checkout before, also with a changed
# registration), and invokes brain.start in each running herdr session. It uses
# no herdr folder layout (internal/update/postinstall.go). herdr gives a build no HERDR_
# variable (herdr plugin docs), so with HERDR_ENV set this is a build by hand in a herdr pane,
# and it starts nothing. One 60 s deadline covers all herdr calls, and each herdr call also
# has a 5 s deadline. The child gets /dev/null for stdin, stdout and stderr: herdr reads the
# build output until EOF. BRAIN_NO_AUTO_SETUP=1 turns it off.
auto_start() {
	[ "${BRAIN_NO_AUTO_SETUP:-}" != 1 ] || return 0
	[ -z "${HERDR_ENV:-}" ] || return 0
	command -v herdr >/dev/null 2>&1 || return 0
	commit=$(git rev-parse -q --verify 'HEAD^{commit}' 2>/dev/null) || return 0
	bin/brain post-install --commit "$commit" </dev/null >/dev/null || :
}

# from_source builds this checkout. Callers in a `||` list turn off set -e, so check go build.
from_source() {
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${version}" -o bin/brain ./cmd/brain ||
		die "go build failed"
	echo "brain build: bin/brain ${version} (built from source)"
	auto_start
	exit 0
}

# not_release: this checkout is not the exact release, so the asset does not apply.
not_release() {
	[ "$has_go" = no ] || from_source
	die "this ref needs Go 1.23 or later ($1). Install Go, then run: sh scripts/build.sh"
}

# no_asset: the release asset cannot be used. With Go, one stderr line and a source build.
# With no Go, it stops: $2 is the end of that error (default "Install stopped.").
no_asset() {
	if [ "$has_go" = yes ]; then
		echo "brain build: $1; building from source" >&2
		from_source
	fi
	die "$1. ${2:-Install stopped.}"
}

command -v git >/dev/null 2>&1 || not_release "git is not installed, so the release tag cannot be checked"
[ -z "$(git status --porcelain 2>/dev/null || echo dirty)" ] || not_release "the checkout has local changes"
head=$(git rev-parse -q --verify 'HEAD^{commit}' 2>/dev/null) || not_release "no git HEAD"

# The commit of the release tag. A herdr managed checkout is a shallow fetch at FETCH_HEAD
# with no tags, so when the local lookup finds nothing, ask origin. For an annotated tag
# the peeled "^{}" line is the commit; a lightweight tag has only the plain line.
tagged=$(git rev-parse -q --verify "refs/tags/${tag}^{commit}" 2>/dev/null) || {
	refs=$(GIT_TERMINAL_PROMPT=0 git ls-remote origin "refs/tags/${tag}" "refs/tags/${tag}^{}" 2>/dev/null) || refs=""
	tagged=$(printf '%s\n' "$refs" | awk -v r="refs/tags/${tag}" '$2 == r "^{}" { p = $1 } $2 == r { l = $1 } END { print (p != "" ? p : l) }')
	[ -n "$tagged" ] || not_release "tag ${tag} is not in this checkout or on origin"
}
[ "$head" = "$tagged" ] || not_release "HEAD is not the commit of tag ${tag}"

install_go="Install Go 1.23 or later, then run: sh scripts/build.sh"
case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) no_asset "no release asset for OS $(uname -s)" "$install_go" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) no_asset "no release asset for CPU $(uname -m)" "$install_go" ;;
esac
asset="brain_${os}_${arch}"

command -v curl >/dev/null 2>&1 || no_asset "curl is not installed; it is needed to download ${asset}" "$install_go"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	no_asset "sha256sum or shasum is needed to check ${asset}" "$install_go"
fi

# BRAIN_RELEASE_BASE_URL is for tests only (a file:// folder in place of GitHub).
base="${BRAIN_RELEASE_BASE_URL:-https://github.com/aryanVijaywargia/herdr-brain/releases/download}/${tag}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# [NEW] 10 s connect timeout: herdr has no build timeout, so a dead network falls back to Go fast.
fetch() { curl -fsSL --connect-timeout 10 -o "$tmp/$1" "$base/$1" 2>/dev/null; }
fetch checksums.txt || no_asset "cannot download $base/checksums.txt"
fetch "$asset" || no_asset "cannot download $base/$asset"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || no_asset "checksums.txt has no entry for ${asset}"
got=$(sha256 "$tmp/$asset")
[ "$got" = "$want" ] || no_asset "checksum mismatch for ${asset} (want ${want}, got ${got})"

chmod +x "$tmp/$asset"
printed=$("$tmp/$asset" version 2>/dev/null) || printed=""
[ "$printed" = "brain ${version}" ] || no_asset "${asset} version printed \"${printed}\", want \"brain ${version}\""

mv "$tmp/$asset" bin/brain
echo "brain build: bin/brain ${version} (release asset ${asset}, checksum OK)"
auto_start
