#!/bin/bash
# Installed as /docker-entrypoint-initdb.d/00_install_pg_cron.sh
#
# This script runs inside the official postgres:16 container as part of
# the one-time initdb phase (executed only when the data directory is
# empty, i.e. on first start).
#
# It installs the postgresql-16-cron package. The pg_cron extension itself
# is created by the application migration.
#
# The required PostgreSQL configuration is passed via -c flags in the
# docker-compose command: override, so no postgresql.conf editing is
# needed here.

set -euo pipefail

echo "[initdb] Installing postgresql-16-cron..."
apt-get update -q
apt-get install -y --no-install-recommends postgresql-16-cron
rm -rf /var/lib/apt/lists/*
echo "[initdb] postgresql-16-cron installed."
