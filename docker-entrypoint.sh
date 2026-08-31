#!/bin/sh
set -eu

# Existing Docker volumes may have been created before the service ran as the
# unprivileged limiter user. Fix ownership once at startup, then drop root.
chown -R limiter:limiter /data
exec su-exec limiter:limiter /app/remnawave-traffic-limiter "$@"
