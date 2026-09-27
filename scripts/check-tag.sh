#!/bin/sh
# Release gate: the tag must be v<manifest version>. release.yml runs it; usage: sh scripts/check-tag.sh <tag>
set -eu
cd "$(dirname "$0")/.."

tag="${1:?usage: sh scripts/check-tag.sh <tag>}"
version=$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml)
if [ "$tag" != "v${version}" ]; then
	echo "release: tag ${tag} does not match the manifest version (want v${version})" >&2
	exit 1
fi
echo "release: tag ${tag} matches the manifest version"
