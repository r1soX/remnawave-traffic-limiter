#!/bin/sh
# Run from the bedolaga-cabinet repository after updating to v1.80.1.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

python3 "$SCRIPT_DIR/apply_bedolaga_cabinet_patch.py"

if command -v npm >/dev/null 2>&1; then
    npm run type-check
else
    echo "WARNING: npm is unavailable; cabinet type-check was skipped." >&2
fi

echo "Bedolaga cabinet paired-device patch applied."
