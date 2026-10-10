# Save each completed install the shared store lacks, atomically (see
# toolcache.go). Two completion conventions: actions/tool-cache's
# <tool>/<version>/<arch> with a sibling <arch>.complete, and an install
# directory at least two levels deep holding its own .complete stamp.
set -eu
src=$ACT_TOOLCACHE_MOUNT
dst=$ACT_TOOLCACHE_STORE
[ -d "$src" ] || exit 0
mkdir -p "$dst" || exit $?
find "$dst" -mindepth 1 -maxdepth 1 -name '.saving-*' -mmin +"$ACT_STALE_STAGE_MINUTES" -exec rm -rf {} + || exit $?
cd "$src" || exit $?
new_stage() {
  tmp=$(mktemp -d "$dst/.saving-XXXXXXXX") || return $?
  mkdir -p "$dst/${dir%/*}" || { code=$?; rm -rf "$tmp"; return "$code"; }
}
pristine() {
  changed=$(find "$dir" -mindepth 1 ! -type d -newer "$1" ! -path '*/__pycache__/*' | head -n 1)
  [ -z "$changed" ] && return 0
  echo "tool cache: not saving $dir: changed after it completed ($changed)" >&2
  return 1
}
for marker in */*/*.complete; do
  [ -f "$marker" ] || continue
  dir=${marker%.complete}
  [ -d "$dir" ] && [ ! -e "$dst/$marker" ] || continue
  if [ -d "$dst/$dir" ]; then cp "$marker" "$dst/$marker" || exit $?; continue; fi
  pristine "$marker" || continue
  new_stage || exit $?
  cp -a "$dir" "$tmp/install" && mv -T "$tmp/install" "$dst/$dir" 2>/dev/null &&
    cp "$marker" "$dst/$marker"
  rm -rf "$tmp"
done
find . -mindepth 3 -type f -name .complete | while read -r stamp; do
  dir=${stamp%/.complete}
  dir=${dir#./}
  [ ! -e "$dst/$dir" ] || continue
  pristine "$stamp" || continue
  new_stage || exit $?
  cp -a "$dir" "$tmp/install" && mv -T "$tmp/install" "$dst/$dir" 2>/dev/null
  rm -rf "$tmp"
done
