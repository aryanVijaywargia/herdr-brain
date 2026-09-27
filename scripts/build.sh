#!/bin/sh
# Builds bin/brain. herdr runs this on `herdr plugin install`; run it yourself before `herdr plugin link .`.
# A prebuilt-release download for users without Go is story E1-S3.
set -eu
cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
	echo "brain build: Go is not installed. Install Go 1.23 or later, then run: sh scripts/build.sh" >&2
	exit 1
fi

version=$(sed -n 's/^version = "\(.*\)"/\1/p' herdr-plugin.toml)
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${version}" -o bin/brain ./cmd/brain
echo "brain build: bin/brain ${version}"
