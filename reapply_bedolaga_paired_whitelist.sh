#!/bin/sh
# Run from ~/remnawave-bedolaga-telegram-bot after every git pull.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SNAPSHOT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/bedolaga-paired-patch.XXXXXX")
PATCH_COMPLETE=0

# Keep the installation atomic.  A Bedolaga update can move one insertion
# point while the earlier patchers still match; without this snapshot that
# leaves the bot half-patched and much harder to recover.  All paths are fixed
# and contain no whitespace, so POSIX word splitting is intentional here.
PATCH_PATHS='app/services/remnawave_service.py
app/cabinet/routes/subscription_modules/traffic.py
app/cabinet/routes/auth.py
app/services/subscription_service.py
app/services/paired_whitelist_limiter.py
app/webserver/remnawave_webhook.py
app/services/remnawave_service.py.before-paired-display
app/cabinet/routes/subscription_modules/traffic.py.before-paired-display
app/cabinet/routes/subscription_modules/traffic.py.before-paired-write-routing
app/cabinet/routes/auth.py.before-paired-write-routing
app/services/subscription_service.py.before-paired-write-routing
app/webserver/remnawave_webhook.py.before-webhook-serialization'

snapshot_index=0
for snapshot_path in $PATCH_PATHS; do
    snapshot_index=$((snapshot_index + 1))
    snapshot_copy="$SNAPSHOT_DIR/files/$snapshot_path"
    mkdir -p "$(dirname -- "$snapshot_copy")"
    if [ -e "$snapshot_path" ]; then
        cp -p "$snapshot_path" "$snapshot_copy"
        : > "$SNAPSHOT_DIR/$snapshot_index.present"
    fi
done

restore_on_failure() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$PATCH_COMPLETE" -ne 1 ]; then
        snapshot_index=0
        for snapshot_path in $PATCH_PATHS; do
            snapshot_index=$((snapshot_index + 1))
            snapshot_copy="$SNAPSHOT_DIR/files/$snapshot_path"
            if [ -f "$SNAPSHOT_DIR/$snapshot_index.present" ]; then
                cp -p "$snapshot_copy" "$snapshot_path"
            else
                rm -f "$snapshot_path"
            fi
        done
        echo "ERROR: patch failed; all Bedolaga files were restored." >&2
    fi
    rm -rf "$SNAPSHOT_DIR"
    exit "$status"
}

trap restore_on_failure EXIT
trap 'exit 1' HUP INT TERM

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

PATCH_COMPLETE=1
echo "Bedolaga paired WhiteList patches applied and syntax-checked."
