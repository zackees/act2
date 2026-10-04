#!/bin/sh
set -eu
test "$STATE_pre" = copied
test "$(cat pre)" = pre
printf changed > changed
rm deleted
printf '#!/bin/sh\nexit 0\n' > added
chmod 751 added
ln -s added linked
mkdir -p commands
cp added commands/bridge-command
printf '%s/commands\n' "$GITHUB_WORKSPACE" >> "$GITHUB_PATH"
printf 'BRIDGE_ENV=copied\n' >> "$GITHUB_ENV"
printf 'result=copied\n' >> "$GITHUB_OUTPUT"
printf 'main=copied\n' >> "$GITHUB_STATE"
printf 'summary copied\n' >> "$GITHUB_STEP_SUMMARY"
