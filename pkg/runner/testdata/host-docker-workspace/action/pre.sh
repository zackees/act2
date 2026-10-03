#!/bin/sh
set -eu
test -f "$GITHUB_EVENT_PATH"
printf pre > pre
printf 'pre=copied\n' >> "$GITHUB_STATE"
