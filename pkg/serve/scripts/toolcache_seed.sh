# Seed the tool-cache volume from the shared store: copy visible published
# entries; hidden save stages and control files stay in the store.
set -eu
src=$ACT_TOOLCACHE_STORE
mkdir -p "$src"
docker volume create "$ACT_TOOLCACHE_VOLUME" >/dev/null
target=$ACT_TOOLCACHE_MOUNT
for entry in "$src"/*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  cp -a "$entry" "$target"/ || exit $?
done
