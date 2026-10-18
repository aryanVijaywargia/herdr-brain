#!/bin/sh
# Prints the license files of every Go module in the brain binary, for the release asset
# THIRD_PARTY_LICENSES.txt. Some of these licenses (BSD) say that a binary must carry their
# notice. release.yml runs it; usage: sh scripts/third-party-licenses.sh > THIRD_PARTY_LICENSES.txt
set -eu
cd "$(dirname "$0")/.."

# The release builds for darwin and linux, so take the modules of both. The main module is
# not in the list: its license is LICENSE, which the release links to.
modules=$(for os in darwin linux; do
	GOOS=$os go list -deps -f '{{if not .Standard}}{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}{{end}}' ./cmd/brain
done | sort -u)
[ -n "$modules" ] || {
	echo "third-party-licenses: go list found no modules" >&2
	exit 1
}

echo "Herdr Brain includes these Go modules. Their license files follow."
echo "The license of Herdr Brain itself is LICENSE in the repo (MIT)."

# The Go standard library is in the binary too.
goroot=$(go env GOROOT)
printf '\n================================================================\n'
printf 'Go standard library %s: LICENSE\n\n' "$(go env GOVERSION)"
cat "$goroot/LICENSE"

printf '%s\n' "$modules" | while read -r path version dir; do
	files=$(ls "$dir" | grep -iE '(licen[cs]e|copying|notice|^authors$)' || :)
	[ -n "$files" ] || {
		echo "third-party-licenses: no license file in $path $version" >&2
		exit 1
	}
	for f in $files; do
		printf '\n================================================================\n'
		printf '%s %s: %s\n\n' "$path" "$version" "$f"
		cat "$dir/$f"
	done
done
