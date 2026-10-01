"""Apply the paired-device UI contract to bedolaga-cabinet 1.80.1.

The backend continues to return the established ``devices/total/device_limit``
shape, while optional paired metadata lets the cabinet explain that Main and
WhiteList connections are counted as one de-duplicated device set.
"""

from __future__ import annotations

import json
from pathlib import Path
import re
import shutil
import sys


PACKAGE = Path("package.json")
API = Path("src/api/subscription.ts")
PANEL = Path("src/components/subscription/manage/DevicesPanel.tsx")
LOCALES = {
    Path("src/locales/ru.json"): "Основная подписка и WhiteList учтены вместе",
    Path("src/locales/en.json"): "Main subscription and WhiteList are counted together",
    Path("src/locales/fa.json"): "اشتراک اصلی و WhiteList با هم محاسبه می‌شوند",
    Path("src/locales/zh.json"): "主订阅和 WhiteList 设备已合并统计",
}


def fail(message: str) -> None:
    print(f"ERROR: {message}", file=sys.stderr)
    raise SystemExit(1)


def replace_once(source: str, old: str, new: str, label: str) -> str:
    count = source.count(old)
    if count != 1:
        fail(f"{label}: expected 1 fragment, found {count}; no files changed")
    return source.replace(old, new, 1)


def main() -> None:
    required = (PACKAGE, API, PANEL, *LOCALES)
    if any(not path.is_file() for path in required):
        fail("run this script from the root of bedolaga-cabinet")

    package = json.loads(PACKAGE.read_text(encoding="utf-8"))
    if package.get("version") != "1.80.1":
        fail(f"expected bedolaga-cabinet 1.80.1, found {package.get('version')!r}")

    originals = {path: path.read_text(encoding="utf-8") for path in required if path != PACKAGE}
    if "pairedDevicesIncluded" in originals[PANEL]:
        print("Bedolaga cabinet paired-device patch is already applied.")
        return

    api_old = """    total: number;
    device_limit: number;
  }> => {
    const response = await apiClient.get(
      '/cabinet/subscription/devices',
"""
    api_new = """    total: number;
    device_limit: number;
    paired?: boolean;
    main_total?: number;
    whitelist_total?: number;
  }> => {
    const response = await apiClient.get(
      '/cabinet/subscription/devices',
"""
    patched: dict[Path, str] = {
        API: replace_once(originals[API], api_old, api_new, "subscription API device metadata")
    }

    panel_pattern = re.compile(
        r'(?P<indent>[ \t]*)<div className="mb-2 font-mono text-\[11px\] text-dark-400">'
        r'(?P<body>.*?)'
        r'(?P=indent)</div>',
        re.DOTALL,
    )
    match = panel_pattern.search(originals[PANEL])
    if match is None:
        fail("DevicesPanel usage counter was not recognized; no files changed")
    indent = match.group("indent")
    body = match.group("body").strip("\n")
    replacement = (
        f'{indent}<div className="mb-2 flex items-center justify-between gap-3 text-[11px] text-dark-400">\n'
        f'{body}\n'
        f'{indent}  {{devicesData.paired && (\n'
        f'{indent}    <span className="text-right text-[10px]">\n'
        f"{indent}      {{t('subscription.pairedDevicesIncluded')}}\n"
        f'{indent}    </span>\n'
        f'{indent}  )}}\n'
        f'{indent}</div>'
    )
    patched[PANEL] = originals[PANEL][: match.start()] + replacement + originals[PANEL][match.end() :]

    for path, translation in LOCALES.items():
        locale_source = originals[path]
        anchor_pattern = re.compile(r'^(?P<indent>[ \t]*)"myDevices":\s*"(?P<value>.*)",[ \t]*$', re.MULTILINE)
        locale_match = anchor_pattern.search(locale_source)
        if locale_match is None:
            fail(f"{path}: myDevices translation was not found; no files changed")
        line = locale_match.group(0)
        insertion = (
            line
            + "\n"
            + locale_match.group("indent")
            + '"pairedDevicesIncluded": '
            + json.dumps(translation, ensure_ascii=False)
            + ","
        )
        patched[path] = locale_source[: locale_match.start()] + insertion + locale_source[locale_match.end() :]

    for path in patched:
        backup = Path(f"{path}.before-paired-device-ui")
        if not backup.exists():
            shutil.copy2(path, backup)
    for path, content in patched.items():
        path.write_text(content, encoding="utf-8")
    print("Bedolaga cabinet paired-device patch applied.")


if __name__ == "__main__":
    main()
