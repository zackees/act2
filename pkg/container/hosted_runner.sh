set -eu
# Only the hosted-VM surrogate is provisioned; container jobs keep image semantics.
if ! id actrunner >/dev/null 2>&1; then
  command -v useradd >/dev/null || { echo "Hosted runner image needs useradd" >&2; exit 1; }
  uid=$(stat -c %u "$1")
  [ "$uid" -ne 0 ] || uid=1001
  # A bound checkout keeps its host owner; do not recursively chown it.
  useradd --no-log-init --non-unique --uid "$uid" --create-home --home-dir /home/actrunner --shell /bin/bash actrunner
fi
[ "$(id -u actrunner)" -ne 0 ] || { echo "Hosted runner user must be non-root" >&2; exit 1; }
# Hosted runners can use their daemon; preserve the mounted socket's metadata.
if [ -S /var/run/docker.sock ]; then
  gid=$(stat -c %g /var/run/docker.sock)
  if ! getent group "$gid" >/dev/null; then
    groupadd --gid "$gid" "actdocker$gid"
  fi
  usermod --append --groups "$gid" actrunner
fi
mkdir -p /home/actrunner /opt/hostedtoolcache /var/run/act
chown actrunner:$(id -gn actrunner) /home/actrunner
if [ -n "$4" ]; then
  chown actrunner:$(id -gn actrunner) "$4"
fi
# Restored completed installs can belong to the preceding root runner.
# Reconcile only the runner's tool and action stores, never arbitrary binds.
for store in "$2" "$3"; do
  [ -z "$store" ] || chown -R actrunner:$(id -gn actrunner) "$store"
done
if command -v sudo >/dev/null; then
  mkdir -p /etc/sudoers.d
  # The UID may already have an image username, so grant sudo by UID.
  printf '#%s ALL=(ALL) NOPASSWD:ALL\n' "$(id -u actrunner)" > /etc/sudoers.d/actrunner
  chmod 440 /etc/sudoers.d/actrunner
fi
