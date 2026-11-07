#!/usr/bin/env bash
# Print the lines the live job runs: a Spanner endpoint image and the host the
# job builds (see scripts/resolve-host.sh).
#
#   scripts/live-lines.sh [HOST_REF]
#
# Every run has the release line, which blocks, and the master line, which
# does not. A named host ref adds a line with that host, for a pull request or
# a manual run whose tests need a host change that master does not have yet: a
# host pull request's branch, or a commit. The ref is the argument, or else the
# first line of .github/host-ref that is neither blank nor a comment, so a pull
# request that needs a host branch says so in a file the review sees. A host
# ref leg blocks like a release leg: the run asked for it.
#
# The image is the Cloud Spanner emulator behind PGAdapter, the only Spanner
# endpoint a container can provide, and the image the provider's release line
# names (internal/backend.EmulatorImage). TestLiveLinesRunTheDeclaredImage
# holds the two together.
#
# Writes lines=<JSON array> to $GITHUB_OUTPUT.
set -euo pipefail

ref=${1:-}
if [[ -z "$ref" && -f .github/host-ref ]]; then
	ref=$(grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' .github/host-ref | head -n 1 | tr -d '[:space:]' || true)
fi

emulator='{"image":"gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2"}'

lines=$(jq -cn --argjson emulator "$emulator" --arg ref "$ref" '
	[$emulator + {host: "release"}, $emulator + {host: "master"}]
	+ if $ref == "" then [] else [$emulator + {host: $ref}] end')
if [[ -n "$ref" ]]; then
	printf 'host ref %s adds a leg\n' "$ref" >&2
fi
printf 'lines=%s\n' "$lines" >>"${GITHUB_OUTPUT:-/dev/stdout}"
