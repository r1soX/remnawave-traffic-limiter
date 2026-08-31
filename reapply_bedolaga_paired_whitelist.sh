#!/bin/sh
# Run from ~/remnawave-bedolaga-telegram-bot after every git pull.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if ! grep -q "async def _paired_limiter_traffic" app/cabinet/routes/subscription_modules/traffic.py; then
    python3 "$SCRIPT_DIR/apply_bedolaga_display_patch.py"
fi

python3 "$SCRIPT_DIR/apply_bedolaga_paired_whitelist.py"
python3 "$SCRIPT_DIR/apply_bedolaga_webhook_serialization.py"

python3 -m py_compile \
    app/services/paired_whitelist_limiter.py \
    app/services/subscription_service.py \
    app/cabinet/routes/auth.py \
    app/cabinet/routes/subscription_modules/traffic.py \
    app/webserver/remnawave_webhook.py

echo "Bedolaga paired WhiteList patches applied and syntax-checked."
