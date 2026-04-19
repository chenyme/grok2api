#!/usr/bin/env sh
set -eu

/app/scripts/init_storage.sh

exec "$@"
