"""Apply the Bedolaga paired-WhiteList write/display integration.

Run from the root of remnawave-bedolaga-telegram-bot after the display patch.
The script is idempotent for its own additions and refuses unknown source layouts.
"""

from pathlib import Path
import sys

ROOT = Path.cwd()
TRAFFIC = ROOT / "app/cabinet/routes/subscription_modules/traffic.py"
STATUS = ROOT / "app/cabinet/routes/subscription_modules/status.py"
AUTH = ROOT / "app/cabinet/routes/auth.py"
SUBS = ROOT / "app/services/subscription_service.py"
PROJECTION = ROOT / "app/services/panel_sync/projection.py"
HELPER = ROOT / "app/services/paired_whitelist_limiter.py"


def die(message: str) -> None:
    print(f"ERROR: {message}", file=sys.stderr)
    raise SystemExit(1)


def once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        die(f"{label}: expected 1 upstream fragment, found {count}; no files changed")
    return text.replace(old, new, 1)


HELPER_SOURCE = r'''"""Paired WhiteList adapter for Bedolaga."""
from __future__ import annotations

import asyncio
import json
import os
from datetime import UTC, datetime
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

import structlog

logger = structlog.get_logger(__name__)


async def get_paired_state(short_uuid: str | None) -> dict[str, Any] | None:
    base_url = os.getenv("PAIRED_WHITELIST_LIMITER_URL", "").rstrip("/")
    token = os.getenv("PAIRED_WHITELIST_LIMITER_TOKEN", "")
    if not base_url or not short_uuid or not token:
        return None

    def fetch() -> dict[str, Any]:
        request = Request(
            f"{base_url}/api/pairing/{quote(short_uuid, safe='')}",
            headers={
                "Accept": "application/json",
                "X-Paired-Whitelist-Token": token,
            },
        )
        with urlopen(request, timeout=3) as response:
            return json.loads(response.read().decode())

    try:
        value = await asyncio.to_thread(fetch)
    except (HTTPError, URLError, OSError, ValueError) as error:
        logger.warning("Could not load paired WhiteList state", error=error)
        return None
    return value if value.get("paired") else None


def paired_traffic(state: dict[str, Any] | None) -> dict[str, float | int] | None:
    traffic = (state or {}).get("traffic")
    if not isinstance(traffic, dict):
        return None
    limit, used = traffic.get("limitBytes"), traffic.get("usedBytes")
    if not isinstance(limit, (int, float)) or not isinstance(used, (int, float)):
        return None
    return {
        "limit_bytes": int(limit),
        "limit_gb": int(limit) / (1024**3) if limit > 0 else 0,
        "used_bytes": int(used),
        "used_gb": int(used) / (1024**3),
    }


def paired_white_user_id(state: dict[str, Any] | None) -> int | None:
    value = (state or {}).get("paired", {}).get("whiteUserId")
    return int(value) if isinstance(value, (int, float)) else None


async def reconcile_pair(short_uuid: str | None) -> bool:
    base_url = os.getenv("PAIRED_WHITELIST_LIMITER_URL", "").rstrip("/")
    token = os.getenv("PAIRED_WHITELIST_LIMITER_TOKEN", "")
    if not base_url or not short_uuid or not token:
        return False

    def reconcile() -> None:
        request = Request(
            f"{base_url}/api/pairing/{quote(short_uuid, safe='')}",
            headers={"X-Paired-Whitelist-Token": token},
        )
        with urlopen(request, timeout=10):
            pass

    try:
        await asyncio.to_thread(reconcile)
        return True
    except (HTTPError, URLError, OSError):
        logger.warning("Could not reconcile paired WhiteList user", short_uuid=short_uuid)
        return False


def paired_tariff_for_subscription(subscription: Any) -> bool | None:
    """Return whether the logical tariff contains both Main and WhiteList."""
    whitelist_uuid = os.getenv("PAIRED_WHITELIST_SQUAD_UUID", "").strip()
    if not whitelist_uuid:
        return None
    tariff = getattr(subscription, "tariff", None)
    squads = getattr(tariff, "allowed_squads", None) if tariff is not None else None
    if not squads:
        squads = getattr(subscription, "connected_squads", None) or []
    normalized = {str(squad).strip() for squad in squads if str(squad).strip()}
    has_main = any(squad != whitelist_uuid for squad in normalized)
    return whitelist_uuid in normalized and has_main


def target_traffic_limit_gb(subscription: Any) -> int:
    """Return the logical quota, falling back to the tariff after a bad panel pull."""
    local_limit = int(getattr(subscription, "traffic_limit_gb", 0) or 0)
    if local_limit > 0:
        return local_limit
    tariff = getattr(subscription, "tariff", None)
    return int(getattr(tariff, "traffic_limit_gb", 0) or 0)


def paired_mode_for_subscription(subscription: Any) -> bool | None:
    """Pair only finite tariffs that contain both Main and WhiteList.

    A WhiteList-only tariff is a normal single Remnawave user: its own native
    counter must account for WhiteList traffic, so creating a Main companion
    would be both unnecessary and wrong.
    """
    paired_tariff = paired_tariff_for_subscription(subscription)
    if paired_tariff is None:
        return None
    return paired_tariff and target_traffic_limit_gb(subscription) > 0


def _target_squads(subscription: Any) -> list[str]:
    tariff = getattr(subscription, "tariff", None)
    values = getattr(tariff, "allowed_squads", None) if tariff is not None else None
    if not values:
        values = getattr(subscription, "connected_squads", None) or []
    return [str(value) for value in values if str(value).strip()]


def reset_is_new_billing_period(reset_traffic: bool, reset_reason: str | None) -> bool:
    """A tariff switch must apply the new quota but preserve WhiteList usage."""
    reason = str(reset_reason or "").casefold()
    return bool(reset_traffic) and "тариф" not in reason and "tariff" not in reason


async def sync_tariff_pairing(
    short_uuid: str | None,
    subscription: Any,
    *,
    reset_white_traffic: bool = False,
    traffic_limit_strategy: str = "MONTH",
) -> bool:
    """Send the complete desired tariff state to the limiter.

    This is deliberately not a "toggle".  The limiter must receive every
    field that defines the original Main account, otherwise a retained
    companion can keep an old 150 GiB limit after the user selected 50 GiB.
    """
    desired = paired_mode_for_subscription(subscription)
    base_url = os.getenv("PAIRED_WHITELIST_LIMITER_URL", "").rstrip("/")
    token = os.getenv("PAIRED_WHITELIST_LIMITER_TOKEN", "")
    if desired is None or not base_url or not short_uuid or not token:
        if desired is not None:
            logger.warning("Paired WhiteList control is not configured", short_uuid=short_uuid)
        return False

    limit_gb = target_traffic_limit_gb(subscription)
    # Bedolaga v4.10+ may already have imported Main's technical zero before
    # this patch was installed. Recover the exact companion quota (including
    # top-ups) when the tariff itself still says that pairing is required.
    if desired and int(getattr(subscription, "traffic_limit_gb", 0) or 0) == 0:
        virtual_traffic = paired_traffic(await get_paired_state(short_uuid))
        if virtual_traffic and int(virtual_traffic["limit_bytes"]) > 0:
            limit_gb = int(virtual_traffic["limit_gb"])

    def send() -> None:
        end_date = getattr(subscription, "end_date", None)
        if end_date is None:
            raise ValueError("subscription end_date is required")
        # Bedolaga passes TrafficLimitStrategy (an Enum) from its service
        # layer. JSON can only carry its wire value, e.g. "MONTH".
        strategy_value = getattr(traffic_limit_strategy, "value", traffic_limit_strategy)
        strategy_text = str(strategy_value or "MONTH")
        current_status = str(getattr(subscription, "status", "") or "").lower()
        is_active = current_status in {"active", "trial"} and end_date > datetime.now(UTC)
        payload: dict[str, Any] = {
            "enabled": desired,
            "trafficLimitBytes": limit_gb * (1024**3),
            "trafficLimitStrategy": strategy_text,
            "expireAt": end_date.isoformat(),
            "status": "ACTIVE" if is_active else "DISABLED",
            "activeInternalSquads": _target_squads(subscription),
            # Only renewal/reset paths pass True. The legacy field name is
            # retained, while the limiter resets Main and WhiteList together.
            # Tariff changes and top-ups preserve spent traffic.
            "resetWhiteTraffic": bool(reset_white_traffic and desired),
        }
        request = Request(
            f"{base_url}/api/pairing/{quote(short_uuid, safe='')}",
            data=json.dumps(payload).encode(),
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Accept": "application/json",
                "X-Paired-Whitelist-Token": token,
            },
        )
        with urlopen(request, timeout=10) as response:
            result = json.loads(response.read().decode())
        if result.get("status") != "ok":
            raise ValueError("limiter rejected target")

    try:
        await asyncio.to_thread(send)
        logger.info(
            "Applied paired WhiteList tariff target",
            short_uuid=short_uuid,
            paired=desired,
            traffic_limit_gb=limit_gb,
            tariff_id=getattr(subscription, "tariff_id", None),
        )
        return True
    except (HTTPError, URLError, OSError, TypeError, ValueError) as error:
        logger.warning("Could not apply paired WhiteList tariff transition", short_uuid=short_uuid, error=error)
        return False
'''


def patch_traffic(source: str) -> str:
    old_state_fetch = """    base_url = os.getenv('PAIRED_WHITELIST_LIMITER_URL', '').rstrip('/')
    if not base_url or not short_uuid:
        return None

    url = f'{base_url}/api/state/{quote(short_uuid, safe="")}'

    def fetch() -> dict[str, Any]:
        request = Request(url, headers={'Accept': 'application/json'})
"""
    new_state_fetch = """    base_url = os.getenv('PAIRED_WHITELIST_LIMITER_URL', '').rstrip('/')
    token = os.getenv('PAIRED_WHITELIST_LIMITER_TOKEN', '')
    if not base_url or not short_uuid or not token:
        return None

    url = f'{base_url}/api/pairing/{quote(short_uuid, safe="")}'

    def fetch() -> dict[str, Any]:
        request = Request(url, headers={
            'Accept': 'application/json',
            'X-Paired-Whitelist-Token': token,
        })
"""
    # Upgrade installations that already received the former display patch.
    # /api/state is intentionally hidden from the public proxy, whereas the
    # authenticated pairing route is the bot's existing control channel.
    source = source.replace(old_state_fetch, new_state_fetch, 1)
    display_only = """        paired_traffic = await _paired_limiter_traffic(panel_short_uuid)
        if paired_traffic:
            traffic_stats = {**(traffic_stats or {}), **paired_traffic}

        if not traffic_stats:
"""
    old_persistent = """        paired_traffic = await _paired_limiter_traffic(panel_short_uuid)
        if paired_traffic:
            traffic_stats = {**(traffic_stats or {}), **paired_traffic}
            virtual_limit_gb = int(paired_traffic['traffic_limit_gb'])
            if subscription.traffic_limit_gb != virtual_limit_gb:
                subscription.traffic_limit_gb = virtual_limit_gb
                subscription.updated_at = datetime.now(UTC)
                await db.commit()

        if not traffic_stats:
"""
    # WhiteList usage is a display overlay. It must never overwrite Bedolaga's
    # billing field: that field is the source of the next tariff target.
    if old_persistent in source:
        patched = source.replace(old_persistent, display_only, 1)
    elif display_only in source:
        patched = source
    else:
        patched = once(source, display_only, display_only, "traffic.py paired traffic overlay")

    old = """        # Проверяем безлимит
        if tariff.traffic_limit_gb == 0:
            return []

        packages = tariff.get_traffic_topup_packages() if hasattr(tariff, 'get_traffic_topup_packages') else {}
"""
    new = """        # The Main account is intentionally unlimited for a paired
        # WhiteList subscription. Its tariff may therefore say unlimited even
        # though the companion has a finite quota. Keep top-up packages visible
        # when the limiter confirms that finite virtual quota.
        if tariff.traffic_limit_gb == 0:
            from app.services.paired_whitelist_limiter import get_paired_state, paired_traffic

            virtual_traffic = paired_traffic(
                await get_paired_state(getattr(subscription, 'remnawave_short_uuid', None))
            )
            if not virtual_traffic or int(virtual_traffic['limit_bytes']) <= 0:
                return []

        packages = tariff.get_traffic_topup_packages() if hasattr(tariff, 'get_traffic_topup_packages') else {}
"""
    if new not in patched:
        patched = once(patched, old, new, "traffic.py paired package visibility")

    old = """        # Проверяем безлимит
        if tariff.traffic_limit_gb == 0:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail='Cannot add traffic to unlimited subscription',
            )

        # Проверяем лимит докупки
"""
    new = """        # Main is unlimited in paired mode; validate the companion's
        # virtual quota instead of rejecting a valid WhiteList traffic top-up.
        if tariff.traffic_limit_gb == 0:
            from app.services.paired_whitelist_limiter import get_paired_state, paired_traffic

            virtual_traffic = paired_traffic(
                await get_paired_state(getattr(subscription, 'remnawave_short_uuid', None))
            )
            if not virtual_traffic or int(virtual_traffic['limit_bytes']) <= 0:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail='Cannot add traffic to unlimited subscription',
                )
            subscription.traffic_limit_gb = int(virtual_traffic['limit_gb'])

        # Проверяем лимит докупки
"""
    if new not in patched:
        patched = once(patched, old, new, "traffic.py paired traffic purchase")
    return patched


def patch_auth(source: str) -> str:
    panel_sync_old = """                snapshot = read_panel_user(panel_user)
                current_time = datetime.now(UTC)
                expire_at = snapshot.expire_at or current_time
                connected_squads = list(snapshot.squads)
                traffic_limit_gb = snapshot.traffic_limit_gb or 0
                traffic_used_gb = snapshot.traffic_used_gb or 0
                device_limit = coerce_panel_device_limit(panel_user.hwid_device_limit, default=0)
"""
    panel_sync_new = """                snapshot = read_panel_user(panel_user)
                current_time = datetime.now(UTC)
                expire_at = snapshot.expire_at or current_time
                connected_squads = list(snapshot.squads)
                traffic_limit_gb = snapshot.traffic_limit_gb or 0
                traffic_used_gb = snapshot.traffic_used_gb or 0

                from dataclasses import replace as dataclass_replace

                from app.services.paired_whitelist_limiter import get_paired_state, paired_traffic

                virtual_traffic = paired_traffic(await get_paired_state(panel_user.short_uuid))
                projection_snapshot = snapshot
                if virtual_traffic:
                    traffic_limit_gb = int(virtual_traffic['limit_gb'])
                    traffic_used_gb = float(virtual_traffic['used_gb'])
                    # Main is deliberately unlimited while the companion owns
                    # the finite counter.  Do not persist that technical Main
                    # state over Bedolaga's tariff fields during email import.
                    projection_snapshot = dataclass_replace(
                        snapshot,
                        traffic_limit_gb=None,
                        traffic_used_gb=None,
                    )

                device_limit = coerce_panel_device_limit(panel_user.hwid_device_limit, default=0)
"""
    if panel_sync_new in source:
        return source
    if panel_sync_old in source:
        source = once(source, panel_sync_old, panel_sync_new, "auth.py panel_sync pairing overlay")
        source = once(
            source,
            """                    project_onto_subscription(
                        existing_sub,
                        snapshot,
""",
            """                    project_onto_subscription(
                        existing_sub,
                        projection_snapshot,
""",
            "auth.py panel_sync persistence guard",
        )
        return source

    old = """                # Parse panel data
                expire_at = panel_datetime_to_utc(panel_user.expire_at)
                traffic_limit_gb = (
                    panel_user.traffic_limit_bytes // (1024**3) if panel_user.traffic_limit_bytes > 0 else 0
                )
                traffic_used_gb = panel_user.used_traffic_bytes / (1024**3) if panel_user.used_traffic_bytes > 0 else 0
"""
    new = """                # Parse panel data
                expire_at = panel_datetime_to_utc(panel_user.expire_at)
                from app.services.paired_whitelist_limiter import get_paired_state, paired_traffic

                virtual_traffic = paired_traffic(await get_paired_state(panel_user.short_uuid))
                paired_virtual_traffic = virtual_traffic is not None
                if virtual_traffic:
                    traffic_limit_gb = int(virtual_traffic['limit_gb'])
                    traffic_used_gb = float(virtual_traffic['used_gb'])
                else:
                    traffic_limit_gb = (
                        panel_user.traffic_limit_bytes // (1024**3) if panel_user.traffic_limit_bytes > 0 else 0
                    )
                    traffic_used_gb = (
                        panel_user.used_traffic_bytes / (1024**3) if panel_user.used_traffic_bytes > 0 else 0
                    )
"""
    previous = """                # Parse panel data
                expire_at = panel_datetime_to_utc(panel_user.expire_at)
                from app.services.paired_whitelist_limiter import get_paired_state, paired_traffic

                virtual_traffic = paired_traffic(await get_paired_state(panel_user.short_uuid))
                if virtual_traffic:
                    traffic_limit_gb = int(virtual_traffic['limit_gb'])
                    traffic_used_gb = float(virtual_traffic['used_gb'])
                else:
                    traffic_limit_gb = (
                        panel_user.traffic_limit_bytes // (1024**3) if panel_user.traffic_limit_bytes > 0 else 0
                    )
                    traffic_used_gb = (
                        panel_user.used_traffic_bytes / (1024**3) if panel_user.used_traffic_bytes > 0 else 0
                    )
"""
    if previous in source:
        source = source.replace(previous, new, 1)
    elif new not in source:
        source = once(source, old, new, "auth.py email pairing overlay")

    old_write = """                    existing_sub.traffic_limit_gb = traffic_limit_gb
                    existing_sub.traffic_used_gb = traffic_used_gb
"""
    guarded_write = """                    # Companion counters are UI data only.  Do not let an
                    # auth/profile refresh overwrite the tariff amount which
                    # must be sent as the next limiter target.
                    if not paired_virtual_traffic:
                        existing_sub.traffic_limit_gb = traffic_limit_gb
                        existing_sub.traffic_used_gb = traffic_used_gb
"""
    if guarded_write not in source:
        source = once(source, old_write, guarded_write, "auth.py virtual traffic persistence guard")
    return source


def patch_projection(source: str) -> str:
    marker = "Paired Main's zero is an implementation detail"
    if marker in source:
        return source

    old = """    moment = now or datetime.now(UTC)
    changed: set[str] = set()

    if snapshot_taken_at is not None:
"""
    new = """    moment = now or datetime.now(UTC)
    changed: set[str] = set()

    from app.services.paired_whitelist_limiter import paired_mode_for_subscription

    if paired_mode_for_subscription(subscription) is True and snapshot.traffic_limit_gb == 0:
        # Paired Main's zero is an implementation detail: the companion owns
        # the real counter and Main carries only its non-WhiteList squads.
        # Bedolaga v4.10+ treats the panel as authoritative, so mask these three
        # technical fields before every shared panel -> bot projection.
        snapshot = replace(
            snapshot,
            traffic_limit_gb=None,
            traffic_used_gb=None,
            squads=(),
        )

    if snapshot_taken_at is not None:
"""
    return once(source, old, new, "panel projection paired Main guard")


def patch_status(source: str) -> str:
    marker = "The first cabinet response must use the companion counter"
    if marker in source:
        return source

    old = """    subscription_data = _subscription_to_response(
        subscription, servers, tariff_name, traffic_purchases_data, user=fresh_user
    )
    return SubscriptionStatusResponse(has_subscription=True, subscription=subscription_data)
"""
    new = """    subscription_data = _subscription_to_response(
        subscription, servers, tariff_name, traffic_purchases_data, user=fresh_user
    )

    from app.services.paired_whitelist_limiter import (
        get_paired_state,
        paired_mode_for_subscription,
        paired_traffic,
    )

    # The first cabinet response must use the companion counter. Waiting for
    # POST /refresh-traffic leaves the page showing Main's technical unlimited
    # value until the user presses refresh.
    virtual_traffic = paired_traffic(
        await get_paired_state(getattr(subscription, 'remnawave_short_uuid', None))
    )
    if virtual_traffic:
        virtual_limit_gb = int(virtual_traffic['limit_gb'])
        virtual_used_gb = float(virtual_traffic['used_gb'])
        virtual_percent = (
            min(100.0, (virtual_used_gb / virtual_limit_gb) * 100) if virtual_limit_gb > 0 else 0.0
        )
        subscription_data = subscription_data.model_copy(
            update={
                'traffic_limit_gb': virtual_limit_gb,
                'traffic_used_gb': round(virtual_used_gb, 2),
                'traffic_used_percent': round(virtual_percent, 1),
            }
        )

        # Repair only the v4.10+ corruption signature: DB says unlimited while
        # the current tariff is finite and paired. Never persist an old quota
        # over a legitimate unlimited tariff or a finite-to-finite switch.
        tariff_limit_gb = int(getattr(getattr(subscription, 'tariff', None), 'traffic_limit_gb', 0) or 0)
        if (
            int(getattr(subscription, 'traffic_limit_gb', 0) or 0) == 0
            and tariff_limit_gb > 0
            and paired_mode_for_subscription(subscription) is True
        ):
            subscription.traffic_limit_gb = virtual_limit_gb
            subscription.traffic_used_gb = virtual_used_gb
            subscription.updated_at = now
            await db.commit()
    elif paired_mode_for_subscription(subscription) is True:
        # Limiter is temporarily unavailable: the finite tariff is still a
        # safer display fallback than Main's synthetic unlimited value.
        fallback_limit_gb = int(getattr(subscription.tariff, 'traffic_limit_gb', 0) or 0)
        fallback_used_gb = float(getattr(subscription, 'traffic_used_gb', 0) or 0)
        fallback_percent = (
            min(100.0, (fallback_used_gb / fallback_limit_gb) * 100) if fallback_limit_gb > 0 else 0.0
        )
        subscription_data = subscription_data.model_copy(
            update={
                'traffic_limit_gb': fallback_limit_gb,
                'traffic_used_gb': round(fallback_used_gb, 2),
                'traffic_used_percent': round(fallback_percent, 1),
            }
        )

    return SubscriptionStatusResponse(has_subscription=True, subscription=subscription_data)
"""
    return once(source, old, new, "cabinet initial paired traffic overlay")


def patch_subscription_service(source: str) -> str:
    method_start = source.find("    async def update_remnawave_user(")
    if method_start < 0:
        die("subscription_service.py update_remnawave_user was not found")
    prefix, source = source[:method_start], source[method_start:]
    helper_import_old = "from app.services.paired_whitelist_limiter import sync_tariff_pairing"
    helper_import_new = (
        "from app.services.paired_whitelist_limiter import "
        "reset_is_new_billing_period, sync_tariff_pairing"
    )
    # The create flow only needs sync_tariff_pairing. Keep its import narrow on
    # repeated runs; reset_is_new_billing_period belongs to the update flow.
    prefix = prefix.replace(helper_import_new, helper_import_old)
    source = source.replace(helper_import_old, helper_import_new)

    old = """                subscription.remnawave_short_uuid = updated_user.short_uuid
                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                if await self._panel_id_is_free_for(db, subscription, updated_user.id):
"""
    new = """                subscription.remnawave_short_uuid = updated_user.short_uuid
                from app.services.paired_whitelist_limiter import sync_tariff_pairing

                await sync_tariff_pairing(
                    updated_user.short_uuid,
                    subscription,
                    traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                )
                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                if await self._panel_id_is_free_for(db, subscription, updated_user.id):
"""
    previous_create_call = """                await sync_tariff_pairing(updated_user.short_uuid, subscription)
"""
    full_create_call = """                await sync_tariff_pairing(
                    updated_user.short_uuid,
                    subscription,
                    traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                )
"""
    if previous_create_call in prefix:
        prefix = prefix.replace(previous_create_call, full_create_call, 1)
    if new not in prefix:
        legacy = """                subscription.remnawave_short_uuid = updated_user.short_uuid
                from app.services.paired_whitelist_limiter import reconcile_pair

                await reconcile_pair(updated_user.short_uuid)
                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                if await self._panel_id_is_free_for(db, subscription, updated_user.id):
"""
        if legacy in prefix:
            prefix = prefix.replace(legacy, new, 1)
        else:
            prefix = once(prefix, old, new, "subscription_service.py create pairing sync")

    # Remove the older split write-routing patch.  It directly updated the
    # companion and then tried to repair Main, which made tariff transitions
    # depend on timing and left old quotas behind.  The limiter now owns both
    # writes from one full desired-state command instead.
    source = source.replace(
        """            from app.services.paired_whitelist_limiter import get_paired_state, paired_white_user_id

            paired_state = await get_paired_state(getattr(subscription, 'remnawave_short_uuid', None))
            paired_white_id = paired_white_user_id(paired_state)
""",
        "",
    )
    source = source.replace(
        """
                if paired_white_id:
                    # Main must remain unlimited and its squads are limiter-owned.
                    update_kwargs['traffic_limit_bytes'] = 0
                    update_kwargs.pop('active_internal_squads', None)
                    update_kwargs.pop('external_squad_uuid', None)
""",
        "",
    )
    source = source.replace(
        """
                if paired_white_id:
                    await api.update_user(
                        user_id=paired_white_id,
                        status=UserStatus.ACTIVE if is_actually_active else UserStatus.DISABLED,
                        expire_at=(
                            subscription.end_date
                            if is_actually_active
                            else max(subscription.end_date, current_time + timedelta(minutes=1))
                        ),
                        traffic_limit_bytes=self._gb_to_bytes(subscription.traffic_limit_gb),
                        traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                    )
""",
        "",
    )
    source = source.replace(
        """                    if paired_white_id:
                        reset_id = paired_white_id
                    elif settings.is_multi_tariff_enabled():
""",
        """                    if settings.is_multi_tariff_enabled():
""",
    )

    desired = """                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                await db.commit()

                from app.services.paired_whitelist_limiter import reset_is_new_billing_period, sync_tariff_pairing

                await sync_tariff_pairing(
                    subscription.remnawave_short_uuid,
                    subscription,
                    reset_white_traffic=reset_is_new_billing_period(reset_traffic, reset_reason),
                    traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                )

                status_text = 'активным' if is_actually_active else 'истёкшим'
"""
    previous_v7 = """                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                await db.commit()

                from app.services.paired_whitelist_limiter import sync_tariff_pairing

                await sync_tariff_pairing(
                    subscription.remnawave_short_uuid,
                    subscription,
                    reset_white_traffic=reset_traffic,
                    traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                )

                status_text = 'активным' if is_actually_active else 'истёкшим'
"""
    # Upgrade a v7 application in place.  The import replacement above may
    # already have widened the import, so accept both spellings.
    if previous_v7 in source:
        source = source.replace(previous_v7, desired, 1)
    else:
        previous_v7_with_new_import = previous_v7.replace(
            "from app.services.paired_whitelist_limiter import sync_tariff_pairing",
            "from app.services.paired_whitelist_limiter import reset_is_new_billing_period, sync_tariff_pairing",
        )
        if previous_v7_with_new_import in source:
            source = source.replace(previous_v7_with_new_import, desired, 1)
    previous_call = """                await sync_tariff_pairing(subscription.remnawave_short_uuid, subscription)
"""
    full_call = """                await sync_tariff_pairing(
                    subscription.remnawave_short_uuid,
                    subscription,
                    reset_white_traffic=reset_is_new_billing_period(reset_traffic, reset_reason),
                    traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                )
"""
    if previous_call in source:
        source = source.replace(previous_call, full_call, 1)
    if desired not in source:
        panel_sync_current = """                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                await link_subscription_panel_identity(db, subscription, remnawave_id)
                await db.commit()

                status_text = 'активным' if is_actually_active else 'истёкшим'
"""
        panel_sync_desired = """                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                await link_subscription_panel_identity(db, subscription, remnawave_id)
                await db.commit()

                from app.services.paired_whitelist_limiter import reset_is_new_billing_period, sync_tariff_pairing

                await sync_tariff_pairing(
                    subscription.remnawave_short_uuid,
                    subscription,
                    reset_white_traffic=reset_is_new_billing_period(reset_traffic, reset_reason),
                    traffic_limit_strategy=get_traffic_reset_strategy(subscription.tariff),
                )

                status_text = 'активным' if is_actually_active else 'истёкшим'
"""
        if panel_sync_current in source:
            source = source.replace(panel_sync_current, panel_sync_desired, 1)

    if desired not in source and panel_sync_desired not in source:
        old = """                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                await db.commit()

                status_text = 'активным' if is_actually_active else 'истёкшим'
"""
        legacy = """                subscription.subscription_url = updated_user.subscription_url
                subscription.subscription_crypto_link = updated_user.happ_crypto_link
                await db.commit()

                status_text = 'активным' if is_actually_active else 'истёкшим'
"""
        if legacy in source:
            source = source.replace(legacy, desired, 1)
        else:
            source = once(source, old, desired, "subscription_service tariff pairing sync")
    return prefix + source


def main() -> None:
    for path in (TRAFFIC, STATUS, AUTH, SUBS, PROJECTION):
        if not path.is_file():
            die("run from ~/remnawave-bedolaga-telegram-bot")

    original = {
        path: path.read_text(encoding="utf-8")
        for path in (TRAFFIC, STATUS, AUTH, SUBS, PROJECTION)
    }
    patched = {
        TRAFFIC: patch_traffic(original[TRAFFIC]),
        STATUS: patch_status(original[STATUS]),
        AUTH: patch_auth(original[AUTH]),
        SUBS: patch_subscription_service(original[SUBS]),
        PROJECTION: patch_projection(original[PROJECTION]),
    }

    for path, text in original.items():
        backup = Path(f"{path}.before-paired-write-routing")
        if not backup.exists():
            backup.write_text(text, encoding="utf-8")
    HELPER.write_text(HELPER_SOURCE, encoding="utf-8")
    for path, text in patched.items():
        path.write_text(text, encoding="utf-8")
    print("Paired WhiteList write/display routing patch applied.")


if __name__ == "__main__":
    main()
