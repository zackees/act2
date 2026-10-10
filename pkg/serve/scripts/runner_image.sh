# Make $ACT_IMAGE_TAG present: loaded from the archive in the shared cache, or
# pulled by digest once, tagged and saved there atomically. With
# ACT_IMAGE_RELOAD=1 (after the loaded image failed its proof) the archive and
# the image are dropped first.
set -eu
tar=$ACT_IMAGE_ARCHIVE
mkdir -p "$(dirname "$tar")"
exec 9>>"$tar.lock"
if [ "${ACT_IMAGE_RELOAD:-0}" = 1 ]; then
  flock -x 9
  rm -f "$tar"
  docker image rm -f "$ACT_IMAGE_TAG" >/dev/null 2>&1 || :
else
  flock -s 9
fi
restore() { [ -f "$tar" ] && docker load -q -i "$tar" >/dev/null; }
if ! restore; then
  flock -x 9
  if ! restore; then
    docker pull -q --platform "$ACT_IMAGE_PLATFORM" "$ACT_IMAGE_REFERENCE" >/dev/null
    docker tag "$ACT_IMAGE_REFERENCE" "$ACT_IMAGE_TAG"
    stage=$(mktemp "$tar.XXXXXXXX")
    trap 'rm -f "$stage"' EXIT
    docker save --platform "$ACT_IMAGE_PLATFORM" -o "$stage" "$ACT_IMAGE_TAG"
    mv "$stage" "$tar"
  fi
fi
docker image inspect "$ACT_IMAGE_TAG" >/dev/null
