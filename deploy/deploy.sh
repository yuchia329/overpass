#!/bin/sh
# Builds the backend for the instance, installs it as a systemd service and
# applies the Traefik route. Safe to rerun: the database is kept.
# Usage: deploy/deploy.sh <ssh-host>
set -eu
host=${1:?usage: deploy/deploy.sh <ssh-host>}
cd "$(dirname "$0")/.."

case $(ssh "$host" uname -m) in
aarch64) goarch=arm64 ;;
x86_64) goarch=amd64 ;;
*) echo "unsupported instance architecture" >&2; exit 1 ;;
esac
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$out/overpass" ./cmd/overpass

# A private directory, so no other user on the instance can swap the files.
dir=$(ssh "$host" mktemp -d)
scp -q "$out/overpass" deploy/overpass.service deploy/ingress.yaml "$host:$dir/"
ssh "$host" "dir=$dir"'
set -eu
trap "rm -rf $dir" EXIT
sudo install -m 755 "$dir/overpass" /usr/local/bin/overpass
sudo install -m 644 "$dir/overpass.service" /etc/systemd/system/overpass.service
sudo systemctl daemon-reload
sudo systemctl enable -q overpass
sudo systemctl restart overpass
sudo k3s kubectl apply -f "$dir/ingress.yaml"
systemctl --no-pager --lines=5 status overpass
'
