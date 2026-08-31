"""Apply the display-only paired WhiteList integration to Bedolaga.

Run this script from the root of remnawave-bedolaga-telegram-bot.
It validates every insertion point before writing and creates .before-paired-display
backups next to both edited files.
"""

from pathlib import Path
import sys


SERVICE_PATH = Path("app/services/remnawave_service.py")
TRAFFIC_PATH = Path("app/cabinet/routes/subscription_modules/traffic.py")


def fail(message: str) -> None:
    print(f"ERROR: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    if not SERVICE_PATH.is_file() or not TRAFFIC_PATH.is_file():
        fail("run this script from ~/remnawave-bedolaga-telegram-bot")

    service_source = SERVICE_PATH.read_text()
    traffic_source = TRAFFIC_PATH.read_text()

    service_needle = """                    'subscription_url': user.subscription_url,
                }"""
    if service_source.count(service_needle) != 2:
        fail("unexpected remnawave_service.py version; no files were changed")
    new_service_source = service_source.replace(
        service_needle,
        """                    'short_uuid': user.short_uuid,
                    'subscription_url': user.subscription_url,
                }""",
    )

    import_needle = "import asyncio\nimport math\n"
    if import_needle not in traffic_source:
        fail("traffic.py import section was not recognized; no files were changed")
    new_traffic_source = traffic_source.replace(
        import_needle,
        """import asyncio
import json
import math
import os
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen
""",
        1,
    )

    header_needle = """REMNAWAVE_SYNC_TIMEOUT = 10.0

router = APIRouter()
"""
    header_replacement = """REMNAWAVE_SYNC_TIMEOUT = 10.0


async def _paired_limiter_traffic(short_uuid: str | None) -> dict[str, Any] | None:
    base_url = os.getenv('PAIRED_WHITELIST_LIMITER_URL', '').rstrip('/')
    if not base_url or not short_uuid:
        return None

    url = f'{base_url}/api/state/{quote(short_uuid, safe="")}'

    def fetch() -> dict[str, Any]:
        request = Request(url, headers={'Accept': 'application/json'})
        with urlopen(request, timeout=3) as response:
            return json.loads(response.read().decode())

    try:
        payload = await asyncio.to_thread(fetch)
    except (HTTPError, URLError, OSError, ValueError) as error:
        logger.warning('Failed to load paired WhiteList traffic', error=error)
        return None

    if not payload.get('paired'):
        return None

    value = payload.get('traffic')
    if not isinstance(value, dict):
        return None

    limit = value.get('limitBytes')
    used = value.get('usedBytes')
    if not isinstance(limit, (int, float)) or not isinstance(used, (int, float)):
        return None

    return {
        'traffic_limit_bytes': int(limit),
        'traffic_limit_gb': int(limit) / (1024**3) if limit > 0 else 0,
        'used_traffic_bytes': int(used),
        'used_traffic_gb': int(used) / (1024**3),
    }


router = APIRouter()
"""
    if header_needle not in new_traffic_source:
        fail("traffic.py header insertion point was not recognized; no files were changed")
    new_traffic_source = new_traffic_source.replace(header_needle, header_replacement, 1)

    lookup_needle = """        if not traffic_stats:
            # Return current database values if RemnaWave unavailable
"""
    lookup_replacement = """        panel_short_uuid = (
            (traffic_stats or {}).get('short_uuid')
            or getattr(subscription, 'remnawave_short_uuid', None)
        )
        paired_traffic = await _paired_limiter_traffic(panel_short_uuid)
        if paired_traffic:
            traffic_stats = {**(traffic_stats or {}), **paired_traffic}

        if not traffic_stats:
            # Return current database values if RemnaWave unavailable
"""
    if lookup_needle not in new_traffic_source:
        fail("traffic.py lookup insertion point was not recognized; no files were changed")
    new_traffic_source = new_traffic_source.replace(lookup_needle, lookup_replacement, 1)

    limit_needle = """        # Calculate percentage
        limit_gb = subscription.traffic_limit_gb or 0
"""
    limit_replacement = """        # Calculate percentage
        limit_gb = traffic_stats.get('traffic_limit_gb', subscription.traffic_limit_gb or 0)
"""
    if limit_needle not in new_traffic_source:
        fail("traffic.py limit calculation was not recognized; no files were changed")
    new_traffic_source = new_traffic_source.replace(limit_needle, limit_replacement, 1)

    for path in (SERVICE_PATH, TRAFFIC_PATH):
        Path(f"{path}.before-paired-display").write_text(path.read_text())

    SERVICE_PATH.write_text(new_service_source)
    TRAFFIC_PATH.write_text(new_traffic_source)
    print("Paired WhiteList display patch applied successfully.")


if __name__ == "__main__":
    main()
