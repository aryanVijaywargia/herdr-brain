#!/bin/sh
# Builds bin/brain. herdr runs this on `herdr plugin install`; run it yourself before `herdr plugin link .`.
#
# Source first: with Go on PATH it always builds this checkout.
# With no Go, it installs the release asset, but only when the checkout is clean
# and HEAD is the commit of tag v<manifest version>. The asset must match checksums.txt.
# The asset path needs a public repo: a private repo cannot serve release assets without auth.
set -eu
cd "$(dirname "$0")/.."

version=$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml)
tag="v${version}"
mkdir -p bin

if command -v go >/dev/null 2>&1; then
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${version}" -o bin/brain ./cmd/brain
	echo "brain build: bin/brain ${version} (built from source)"
	exit 0
fi

die() {
	echo "brain build: $*" >&2
	exit 1
}

needs_go() {
	die "this ref needs Go 1.23 or later ($1). Install Go, then run: sh scripts/build.sh"
}

command -v git >/dev/null 2>&1 || needs_go "git is not installed, so the release tag cannot be checked"
[ -z "$(git status --porcelain 2>/dev/null || echo dirty)" ] || needs_go "the checkout has local changes"
head=$(git rev-parse -q --verify 'HEAD^{commit}' 2>/dev/null) || needs_go "no git HEAD"
tagged=$(git rev-parse -q --verify "refs/tags/${tag}^{commit}" 2>/dev/null) || needs_go "tag ${tag} is not in this checkout"
[ "$head" = "$tagged" ] || needs_go "HEAD is not the commit of tag ${tag}"

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "no release asset for OS $(uname -s). Install Go 1.23 or later, then run: sh scripts/build.sh" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "no release asset for CPU $(uname -m). Install Go 1.23 or later, then run: sh scripts/build.sh" ;;
esac
asset="brain_${os}_${arch}"

command -v curl >/dev/null 2>&1 || die "curl is not installed; it is needed to download ${asset}"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	die "sha256sum or shasum is needed to check ${asset}"
fi

# BRAIN_RELEASE_BASE_URL is for tests only (a file:// folder in place of GitHub).
base="${BRAIN_RELEASE_BASE_URL:-https://github.com/aryanVijaywargia/herdr-brain/releases/download}/${tag}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "cannot download $base/checksums.txt"
curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "cannot download $base/$asset"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "checksums.txt has no entry for ${asset}. Install stopped."
got=$(sha256 "$tmp/$asset")
[ "$got" = "$want" ] || die "checksum mismatch for ${asset} (want ${want}, got ${got}). Install stopped."

chmod +x "$tmp/$asset"
mv "$tmp/$asset" bin/brain
echo "brain build: bin/brain ${version} (release asset ${asset}, checksum OK)"
