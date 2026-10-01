"""Patch Bedolaga's cabinet device routes for a paired WhiteList account.

Run from the root of remnawave-bedolaga-telegram-bot after
``apply_bedolaga_paired_whitelist.py``.  The logical subscription owns both
panel identities, so device reads are de-duplicated by HWID and destructive
operations are applied to Main and WhiteList together.
"""

from __future__ import annotations

from pathlib import Path
import shutil
import sys


PATH = Path("app/cabinet/routes/subscription_modules/devices.py")


def fail(message: str) -> None:
    print(f"ERROR: {message}", file=sys.stderr)
    raise SystemExit(1)


def function_block(source: str, start: str, end: str, label: str) -> tuple[str, str, str]:
    start_at = source.find(start)
    if start_at < 0:
        fail(f"{label}: function start was not found; no files changed")
    end_at = len(source) if not end else source.find(end, start_at + len(start))
    if end_at < 0:
        fail(f"{label}: function end was not found; no files changed")
    return source[:start_at], source[start_at:end_at], source[end_at:]


def replace_once(block: str, old: str, new: str, label: str) -> str:
    count = block.count(old)
    if count != 1:
        fail(f"{label}: expected 1 fragment, found {count}; no files changed")
    return block.replace(old, new, 1)


def patch_function(source: str, start: str, end: str, changes: list[tuple[str, str, str]], label: str) -> str:
    prefix, block, suffix = function_block(source, start, end, label)
    for old, new, fragment_label in changes:
        block = replace_once(block, old, new, f"{label} {fragment_label}")
    return prefix + block + suffix


HELPERS = r'''

async def _paired_panel_user_ids(subscription: Subscription, user: User) -> list[int]:
    """Return the panel identities that form one logical subscription."""
    main_user_id = _resolve_panel_user_id(subscription, user)
    if not main_user_id:
        return []

    panel_user_ids = [int(main_user_id)]
    try:
        from app.services.paired_whitelist_limiter import get_paired_state, paired_white_user_id

        state = await get_paired_state(getattr(subscription, 'remnawave_short_uuid', None))
        white_user_id = paired_white_user_id(state)
        if white_user_id and white_user_id not in panel_user_ids:
            panel_user_ids.append(white_user_id)
    except Exception as error:
        # Device management must remain available if the limiter is briefly
        # unavailable; in that case the original Main account is still safe.
        logger.warning('Failed to resolve paired device identities', error=str(error)[:200])
    return panel_user_ids


def _device_hwid(device: dict[str, Any]) -> str | None:
    value = device.get('hwid') or device.get('deviceId') or device.get('id')
    return str(value).strip() if value is not None and str(value).strip() else None


async def _load_logical_devices(api: Any, panel_user_ids: list[int]) -> tuple[list[dict[str, Any]], list[int]]:
    """Load and de-duplicate Main/WhiteList devices by physical HWID."""
    devices_by_hwid: dict[str, dict[str, Any]] = {}
    source_totals: list[int] = []
    for panel_user_id in panel_user_ids:
        response = await api.get_user_devices_all(panel_user_id)
        devices = list((response or {}).get('devices') or [])
        total = (response or {}).get('total')
        source_totals.append(int(total) if isinstance(total, int) else len(devices))
        for device in devices:
            if not isinstance(device, dict):
                continue
            hwid = _device_hwid(device)
            if hwid and hwid not in devices_by_hwid:
                devices_by_hwid[hwid] = device
    return list(devices_by_hwid.values()), source_totals


async def _logical_hwid_belongs_to_subscription(subscription: Subscription, user: User, hwid: str) -> bool:
    from app.services.remnawave_service import RemnaWaveService

    panel_user_ids = await _paired_panel_user_ids(subscription, user)
    if not panel_user_ids:
        return False
    service = RemnaWaveService()
    async with service.get_api_client() as api:
        devices, _ = await _load_logical_devices(api, panel_user_ids)
    return any(_device_hwid(device) == hwid for device in devices)


async def _remove_logical_device(api: Any, panel_user_ids: list[int], hwid: str) -> bool:
    results = [await api.remove_device(panel_user_id, hwid) for panel_user_id in panel_user_ids]
    return bool(results) and all(results)


async def _reset_logical_devices(api: Any, panel_user_ids: list[int]) -> bool:
    results = [await api.reset_user_devices(panel_user_id) for panel_user_id in panel_user_ids]
    return bool(results) and all(results)
'''


def patch(source: str) -> str:
    if "async def _paired_panel_user_ids(" in source:
        return source

    source = source.replace(
        "from app.cabinet.utils.device_ownership import verify_hwid_belongs_to_user\n",
        "",
        1,
    )
    helper_anchor = "\n\n@router.post('/devices')\n"
    if source.count(helper_anchor) != 1:
        fail("devices.py helper insertion point was not recognized; no files changed")
    source = source.replace(helper_anchor, HELPERS + helper_anchor, 1)

    source = patch_function(
        source,
        "@router.get('/devices')\nasync def get_devices(",
        "\n\nclass DeviceRenameRequest",
        [
            (
                "    _panel_user_id = _resolve_panel_user_id(subscription, user)\n    if not _panel_user_id:\n",
                "    panel_user_ids = await _paired_panel_user_ids(subscription, user)\n    if not panel_user_ids:\n",
                "identity lookup",
            ),
            (
                "            response = await api.get_user_devices_all(_panel_user_id)\n\n            devices_list = response.get('devices', [])\n",
                "            devices_list, source_totals = await _load_logical_devices(api, panel_user_ids)\n",
                "logical device load",
            ),
            (
                "                'total': response.get('total', len(formatted_devices)),\n                'device_limit': subscription.device_limit or 0,\n",
                "                'total': len(formatted_devices),\n                'device_limit': subscription.device_limit or 0,\n                'paired': len(panel_user_ids) > 1,\n                'main_total': source_totals[0] if source_totals else 0,\n                'whitelist_total': source_totals[1] if len(source_totals) > 1 else 0,\n",
                "logical totals",
            ),
        ],
        "get_devices",
    )

    source = patch_function(
        source,
        "@router.patch('/devices/{hwid}/name')\nasync def rename_device(",
        "\n\n@router.delete('/devices/{hwid}')",
        [
            (
                "    if not await verify_hwid_belongs_to_user(user, hwid):\n",
                "    if not await _logical_hwid_belongs_to_subscription(subscription, user, hwid):\n",
                "paired ownership",
            )
        ],
        "rename_device",
    )

    source = patch_function(
        source,
        "@router.delete('/devices/{hwid}')\nasync def delete_device(",
        "\n\n@router.delete('/devices')",
        [
            (
                "    _panel_user_id = _resolve_panel_user_id(subscription, user)\n    if not _panel_user_id:\n",
                "    panel_user_ids = await _paired_panel_user_ids(subscription, user)\n    if not panel_user_ids:\n",
                "identity lookup",
            ),
            (
                "            removed = await api.remove_device(_panel_user_id, hwid)\n",
                "            removed = await _remove_logical_device(api, panel_user_ids, hwid)\n",
                "paired delete",
            ),
        ],
        "delete_device",
    )

    delete_all_old = """            # Get all devices first
            response = await api.get_user_devices_all(_panel_user_id)

            if not response:
                return {
                    'success': True,
                    'message': 'No devices to delete',
                    'deleted_count': 0,
                }

            devices_list = response.get('devices', [])
            if not devices_list:
                return {
                    'success': True,
                    'message': 'No devices to delete',
                    'deleted_count': 0,
                }
"""
    delete_all_new = """            devices_list, _ = await _load_logical_devices(api, panel_user_ids)
            if not devices_list:
                return {
                    'success': True,
                    'message': 'No devices to delete',
                    'deleted_count': 0,
                }
"""
    source = patch_function(
        source,
        "@router.delete('/devices')\nasync def delete_all_devices(",
        "\n\n# ============ Device Reduction",
        [
            (
                "    _panel_user_id = _resolve_panel_user_id(subscription, user)\n    if not _panel_user_id:\n",
                "    panel_user_ids = await _paired_panel_user_ids(subscription, user)\n    if not panel_user_ids:\n",
                "identity lookup",
            ),
            (delete_all_old, delete_all_new, "logical device load"),
            (
                "            if not await api.reset_user_devices(_panel_user_id):\n",
                "            if not await _reset_logical_devices(api, panel_user_ids):\n",
                "paired reset",
            ),
        ],
        "delete_all_devices",
    )

    source = patch_function(
        source,
        "@router.get('/devices/reduction-info')\nasync def get_device_reduction_info(",
        "\n\n@router.post('/devices/reduce')",
        [
            (
                "    _panel_user_id = _resolve_panel_user_id(subscription, user)\n    if _panel_user_id:\n",
                "    panel_user_ids = await _paired_panel_user_ids(subscription, user)\n    if panel_user_ids:\n",
                "identity lookup",
            ),
            (
                "                response = await api.get_user_devices_all(_panel_user_id)\n                if response:\n                    connected_devices_count = response.get('total', 0)\n",
                "                devices_list, _ = await _load_logical_devices(api, panel_user_ids)\n                connected_devices_count = len(devices_list)\n",
                "logical count",
            ),
        ],
        "get_device_reduction_info",
    )

    reduce_old = """                response = await api.get_user_devices_all(_panel_user_id)
                if response:
                    devices_list = response.get('devices', [])
                    connected_devices_count = len(devices_list)

                    # If connected devices exceed new limit, remove excess (last connected)
                    if connected_devices_count > new_device_limit:
                        devices_to_remove = connected_devices_count - new_device_limit
                        logger.info(
                            'Removing excess devices for user had new limit',
                            devices_to_remove=devices_to_remove,
                            user_id=user.id,
                            connected_devices_count=connected_devices_count,
                            new_device_limit=new_device_limit,
                        )

                        # Sort by date (oldest first) and remove the last ones
                        sorted_devices = sorted(
                            devices_list,
                            key=lambda d: d.get('updatedAt') or d.get('createdAt') or '\\xff',
                        )
                        devices_to_delete = sorted_devices[-devices_to_remove:]

                        for device in devices_to_delete:
                            device_hwid = device.get('hwid')
                            if device_hwid:
                                try:
                                    if await api.remove_device(_panel_user_id, device_hwid):
                                        devices_removed_count += 1
                                        logger.info('Removed device for user', device_hwid=device_hwid, user_id=user.id)
                                except Exception as del_error:
                                    logger.error('Error removing device', device_hwid=device_hwid, del_error=del_error)
"""
    reduce_new = """                devices_list, _ = await _load_logical_devices(api, panel_user_ids)
                connected_devices_count = len(devices_list)

                # If the logical Main + WhiteList device set exceeds the new
                # limit, remove the newest excess HWIDs from both identities.
                if connected_devices_count > new_device_limit:
                    devices_to_remove = connected_devices_count - new_device_limit
                    logger.info(
                        'Removing excess paired devices after limit reduction',
                        devices_to_remove=devices_to_remove,
                        user_id=user.id,
                        connected_devices_count=connected_devices_count,
                        new_device_limit=new_device_limit,
                    )
                    sorted_devices = sorted(
                        devices_list,
                        key=lambda d: d.get('updatedAt') or d.get('createdAt') or '\\xff',
                    )
                    for device in sorted_devices[-devices_to_remove:]:
                        device_hwid = _device_hwid(device)
                        if device_hwid:
                            try:
                                if await _remove_logical_device(api, panel_user_ids, device_hwid):
                                    devices_removed_count += 1
                                    logger.info('Removed paired device', device_hwid=device_hwid, user_id=user.id)
                            except Exception as del_error:
                                logger.error('Error removing paired device', device_hwid=device_hwid, del_error=del_error)
"""
    source = patch_function(
        source,
        "@router.post('/devices/reduce')\nasync def reduce_devices(",
        "",
        [
            (
                "    _panel_user_id = _resolve_panel_user_id(subscription, user)\n    if _panel_user_id:\n",
                "    panel_user_ids = await _paired_panel_user_ids(subscription, user)\n    if panel_user_ids:\n",
                "identity lookup",
            ),
            (reduce_old, reduce_new, "paired excess removal"),
        ],
        "reduce_devices",
    )

    return source


def main() -> None:
    if not PATH.is_file():
        fail("run this script from the root of remnawave-bedolaga-telegram-bot")

    original = PATH.read_text(encoding="utf-8")
    if "async def _paired_panel_user_ids(" in original:
        print("Paired WhiteList device sync patch is already applied.")
        return

    patched = patch(original)
    backup = PATH.with_suffix(".py.before-paired-device-sync")
    if not backup.exists():
        shutil.copy2(PATH, backup)
    PATH.write_text(patched, encoding="utf-8")
    print("Paired WhiteList device sync patch applied.")


if __name__ == "__main__":
    main()
