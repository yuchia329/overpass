#!/bin/sh
# One-time move of an instance deployed under the project's old name,
# Overpass, to Unstuck. Run as root by deploy.sh before the new unit is
# installed. Does nothing once the old unit is gone, so reruns are safe.
#
# The database is copied to /var/backups before it is moved. Its tables are
# unchanged; only the directory and file names change.
set -eu

[ -f /etc/systemd/system/overpass.service ] || exit 0

systemctl disable --now overpass

# DynamicUser keeps the state in /var/lib/private/<name>, with a symlink at
# /var/lib/<name>. Move the real directory, wherever it is.
for parent in /var/lib/private /var/lib; do
	old="$parent/overpass"
	new="$parent/unstuck"
	if [ -d "$old" ] && [ ! -L "$old" ]; then
		if [ -e "$new" ]; then
			echo "migrate: $new already exists; leaving $old in place" >&2
			exit 1
		fi
		mkdir -p /var/backups
		cp -a "$old" "/var/backups/overpass-$(date +%Y%m%d%H%M%S)"
		mv "$old" "$new"
		for f in "$new"/overpass.db "$new"/overpass.db-wal "$new"/overpass.db-shm; do
			if [ -e "$f" ]; then mv "$f" "$new/unstuck.db${f##*/overpass.db}"; fi
		done
		echo "migrate: moved $old to $new"
	fi
done
rm -f /var/lib/overpass

rm -f /etc/systemd/system/overpass.service /usr/local/bin/overpass
systemctl daemon-reload
k3s kubectl delete namespace overpass --ignore-not-found
