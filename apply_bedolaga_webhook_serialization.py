"""Serialize Bedolaga's database-backed RemnaWave webhook handling.

The lock is acquired before AsyncSessionLocal is opened, so a burst waits
without consuming database connections or holding open transactions.
"""

from __future__ import annotations

from pathlib import Path
import shutil
import sys


PATH = Path("app/webserver/remnawave_webhook.py")


def fail(message: str) -> None:
    print(f"ERROR: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    if not PATH.is_file():
        fail("run from ~/remnawave-bedolaga-telegram-bot")

    text = PATH.read_text()
    if "user_event_lock = asyncio.Lock()" in text:
        print("Webhook serialization patch is already applied.")
        return

    import_marker = "from __future__ import annotations\n\n"
    router_marker = """    router = APIRouter()
    if webhook_service is None:
"""
    handler_marker = """        try:
            async with AsyncSessionLocal() as db:
                try:
                    processed = await webhook_service.process_event(db, event_name, data)
                    await db.commit()
                    return JSONResponse({'status': 'ok', 'processed': processed})
                except Exception:
                    await db.rollback()
                    logger.exception('RemnaWave webhook processing error', event_name=event_name)
                    return JSONResponse({'status': 'ok', 'processed': False})
        except Exception:
"""
    if text.count(import_marker) != 1 or text.count(router_marker) != 1 or text.count(handler_marker) != 1:
        fail("unexpected remnawave_webhook.py version; no files were changed")

    patched = text.replace(import_marker, import_marker + "import asyncio\n", 1)
    patched = patched.replace(
        router_marker,
        """    router = APIRouter()

    # Wait before opening a database session. The current Bedolaga webhook
    # service is single-worker, so one consumer prevents row-lock storms.
    user_event_lock = asyncio.Lock()

    if webhook_service is None:
""",
        1,
    )
    patched = patched.replace(
        handler_marker,
        """        try:
            async with user_event_lock:
                async with AsyncSessionLocal() as db:
                    try:
                        processed = await webhook_service.process_event(db, event_name, data)
                        await db.commit()
                        return JSONResponse({'status': 'ok', 'processed': processed})
                    except Exception:
                        await db.rollback()
                        logger.exception('RemnaWave webhook processing error', event_name=event_name)
                        return JSONResponse({'status': 'ok', 'processed': False})
        except Exception:
""",
        1,
    )
    shutil.copy2(PATH, PATH.with_suffix(".py.before-webhook-serialization"))
    PATH.write_text(patched)
    print("Webhook serialization patch applied.")


if __name__ == "__main__":
    main()
