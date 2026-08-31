# Production-развёртывание: Remnawave + Limiter + Bedolaga

Эта инструкция описывает схему из двух серверов без привязки к конкретному
провайдеру, домену или IP-адресу.

```text
Пользователь / VPN-клиент
          |
          v
SUBSCRIPTION_PUBLIC_DOMAIN  ── сервер подписок ── Subscription Page + Bedolaga
          |                                      ^
          | HTTPS                                | служебный запрос
          v                                      |
LIMITER_PUBLIC_DOMAIN       ── сервер limiter ── Remnawave + Traffic Limiter
```

## Роли серверов

| Сервер | Компоненты | Внешне доступно |
| --- | --- | --- |
| **Limiter** | Remnawave Panel, Traffic Limiter, Caddy | Только `/webhook`, `/sub/*`, `/api/pairing/*` на домене limiter. |
| **Подписки** | Bedolaga, Remnawave Subscription Page, Caddy | Обычные ссылки пользователей и ассеты Subscription Page. |

Диагностика limiter (`/api/state/*`, `/api/reconcile/*`, `/health`, `/ready`)
не должна публиковаться через интернет. Она вызывается локально по SSH.

## 1. Подготовка DNS и секретов

Создайте два HTTPS-домена:

- `LIMITER_PUBLIC_DOMAIN` — указывает на сервер limiter;
- `SUBSCRIPTION_PUBLIC_DOMAIN` — указывает на сервер подписок.

Для каждого секрета используйте отдельное случайное значение:

```bash
openssl rand -hex 32
```

Нужны три разных значения:

| Переменная | Кто использует |
| --- | --- |
| `WEBHOOK_SECRET` | Remnawave Panel и limiter. |
| `PAIRED_WHITELIST_CONTROL_TOKEN` | Limiter и Bedolaga. |
| `DIAGNOSTICS_TOKEN` | Только администратор для локальной диагностики. |

Не помещайте эти значения в Git, документацию, скриншоты или сообщения в
публичных чатах.

> Важно: limiter запрашивает Subscription Page через
> `SUBSCRIPTION_PUBLIC_DOMAIN`. Caddy на сервере подписок должен видеть реальный
> IP сервера limiter для правила `remote_ip`. Если между ними стоит CDN или
> другой прокси, используйте прямую DNS-запись/защищённую частную сеть либо
> настройте доверенный прокси так, чтобы это ограничение не обходилось.

## 2. Установка limiter на сервере limiter

Клонируйте репозиторий и создайте локальный файл окружения:

```bash
git clone <REPOSITORY_URL> remnawave-traffic-limiter
cd remnawave-traffic-limiter
cp .env.example .env
chmod 600 .env
nano .env
```

Минимальный production-вариант `.env`:

```env
REMNAWAVE_PANEL_URL=https://PANEL_PUBLIC_DOMAIN
REMNAWAVE_API_TOKEN=REPLACE_WITH_PANEL_API_TOKEN
WEBHOOK_SECRET=REPLACE_WITH_WEBHOOK_SECRET
DIAGNOSTICS_TOKEN=REPLACE_WITH_DIAGNOSTICS_TOKEN

BASIC_SQUAD_UUID=UUID_MAIN
WHITELIST_SQUAD_UUID=UUID_WHITELIST
LIMIT_NOTICE_SQUAD_UUID=UUID_LIMIT_NOTICE

ENABLE_PAIRED_WHITELIST=true
# Во время пилота укажите один shortUuid. После пилота см. раздел 8.
PAIRED_WHITELIST_USER_SHORT_UUIDS=TEST_USER_SHORT_UUID
PAIRED_WHITELIST_MANAGE_ALL=false
PAIRED_WHITELIST_CONTROL_TOKEN=REPLACE_WITH_CONTROL_TOKEN

SUBSCRIPTION_GATEWAY_ENABLED=true
SUBSCRIPTION_UPSTREAM_URL=https://SUBSCRIPTION_PUBLIC_DOMAIN

# Docker-порт остаётся только на localhost.
BIND_ADDRESS=127.0.0.1
PORT=8080
```

Запустите сервис и убедитесь, что healthcheck работает локально:

```bash
docker compose up -d --build
docker compose ps
curl -fsS http://127.0.0.1:8080/health
```

Если выбран другой `PORT`, используйте его и в локальной проверке, и в Caddy.

## 3. Публикация limiter через Caddy

Скопируйте [Caddyfile.production.example](Caddyfile.production.example) в
конфигурацию Caddy на сервере limiter, замените `LIMITER_PUBLIC_DOMAIN` и,
если необходимо, `127.0.0.1:8080`.

Шаблон публикует только необходимые маршруты:

```text
POST /webhook
GET  /sub/<SHORT_UUID>[/*]
POST /api/pairing/<SHORT_UUID>
```

Проверьте и примените конфигурацию. Если Caddy работает в контейнере, замените
`<CADDY_CONTAINER>` на его имя:

```bash
docker exec <CADDY_CONTAINER> caddy validate \
  --config /etc/caddy/Caddyfile --adapter caddyfile
docker exec <CADDY_CONTAINER> caddy reload \
  --config /etc/caddy/Caddyfile --adapter caddyfile
```

С другой машины убедитесь, что диагностика не опубликована:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://LIMITER_PUBLIC_DOMAIN/api/state/test
# Должно быть: 404
```

В панели Remnawave включите webhook и укажите:

```text
https://LIMITER_PUBLIC_DOMAIN/webhook
```

Его секрет должен совпадать с `WEBHOOK_SECRET` в `.env` limiter.

## 4. Настройка Caddy на сервере подписок

Subscription Page должна слушать только локально, например
`127.0.0.1:3010`. Добавьте в Caddy на сервере подписок отдельный блок:

```caddy
SUBSCRIPTION_PUBLIC_DOMAIN {
    encode zstd gzip

    # Этот маршрут предназначен только для запроса с сервера limiter.
    # Он предотвращает зацикливание: limiter получает исходные Main и
    # WhiteList-подписки прямо у Subscription Page.
    @limiter_upstream {
        header X-Remnawave-Limiter-Gateway 1
        remote_ip LIMITER_SERVER_PUBLIC_IP
    }
    handle @limiter_upstream {
        reverse_proxy 127.0.0.1:3010 {
            header_up Host {host}
            header_up X-Real-IP {remote_host}
            header_up X-Forwarded-For {remote_host}
            header_up X-Forwarded-Host {host}
            header_up X-Forwarded-Proto https
        }
    }

    # Обычная ссылка и все поддерживаемые явные форматы идут в limiter.
    # Последняя часть совместима со ссылками некоторых ботов вида
    # /SHORT_UUID&name=..., где параметры ошибочно добавлены без символа ?.
    @limiter_subscription path_regexp subscription ^/([A-Za-z0-9_-]+)(/(json|v2ray-json|clash|singbox|mihomo|stash))?(&.*)?$
    handle @limiter_subscription {
        rewrite * /sub/{re.subscription.1}{re.subscription.2}
        reverse_proxy https://LIMITER_PUBLIC_DOMAIN {
            header_up Host LIMITER_PUBLIC_DOMAIN
        }
    }

    # Страница в браузере, JavaScript/CSS и динамический app-config остаются
    # у исходной Subscription Page.
    handle {
        reverse_proxy 127.0.0.1:3010 {
            header_up Host {host}
            header_up X-Real-IP {remote_host}
            header_up X-Forwarded-For {remote_host}
            header_up X-Forwarded-Host {host}
            header_up X-Forwarded-Proto https
        }
    }
}
```

Не настраивайте для `/assets/*` отдельный `file_server`. Subscription Page
самостоятельно выдаёт browser session-cookie и динамический маршрут
`/assets/.app-config-v2.json`; статическая подмена ломает веб-страницу.

После изменения конфигурации проверьте Caddy и перезагрузите его теми же
командами из предыдущего раздела.

## 5. Форматы подписки

Обычная ссылка `/SHORT_UUID` использует User-Agent клиента для выбора шаблона
Remnawave. Для диагностики или нераспознанного клиента доступны явные суффиксы:

| Ссылка | Формат |
| --- | --- |
| `/SHORT_UUID` | Автоматический выбор или Base64/URI. |
| `/SHORT_UUID/mihomo`, `/clash`, `/stash` | YAML для Mihomo/Clash/Stash. |
| `/SHORT_UUID/singbox`, `/json`, `/v2ray-json` | JSON для sing-box/Xray. |

Limiter объединяет Main и WhiteList структурно: URI-списки, YAML-прокси и
группы, JSON-outbounds и селекторы. Конфликт одинаковых имён узлов или
непрозрачный зашифрованный ответ возвращает `501`, а не повреждённую
конфигурацию.

## 6. Интеграция Bedolaga

Установите патч по инструкции
[BEDOLAGA_PATCH_INSTALL.md](BEDOLAGA_PATCH_INSTALL.md). В `.env` Bedolaga
укажите:

```env
PAIRED_WHITELIST_LIMITER_URL=https://LIMITER_PUBLIC_DOMAIN
PAIRED_WHITELIST_SQUAD_UUID=UUID_WHITELIST
PAIRED_WHITELIST_LIMITER_TOKEN=THE_SAME_PAIRED_WHITELIST_CONTROL_TOKEN
```

Пересобирайте только сервис бота, используя compose-файлы своей установки.
После каждого обновления Bedolaga заново запускайте скрипт применения патча:

```bash
sh <PATCH_DIRECTORY>/reapply_bedolaga_paired_whitelist.sh
```

## 7. Пилотная проверка

До массового включения оставьте:

```env
PAIRED_WHITELIST_MANAGE_ALL=false
PAIRED_WHITELIST_USER_SHORT_UUIDS=TEST_USER_SHORT_UUID
```

Проверьте последовательно:

1. У пользователя тариф Main + WhiteList с конечным лимитом.
2. В панели создана только одна техническая учётная запись `WL_LIMITER`.
3. Обычная ссылка и веб-страница подписки возвращают `200`.
4. Используемые клиентские форматы возвращают ожидаемый Content-Type:

   ```bash
   curl -sS -o /dev/null -w 'mihomo: %{http_code} %{content_type}\n' \
     https://SUBSCRIPTION_PUBLIC_DOMAIN/TEST_USER_SHORT_UUID/mihomo
   curl -sS -o /dev/null -w 'singbox: %{http_code} %{content_type}\n' \
     https://SUBSCRIPTION_PUBLIC_DOMAIN/TEST_USER_SHORT_UUID/singbox
   ```

5. В каждом клиенте видны Main и WhiteList-конфигурации.
6. У WhiteList исчерпан лимит: отключается только WhiteList, Main остаётся
   доступным.
7. Переключение тарифа Main + WhiteList → Main-only → Main + WhiteList меняет
   лимит и переиспользует техническую пару без создания новых аккаунтов.

Локальная диагностика на сервере limiter:

```bash
curl -sS \
  -H "X-Limiter-Diagnostics-Token: $DIAGNOSTICS_TOKEN" \
  http://127.0.0.1:8080/api/state/TEST_USER_SHORT_UUID
```

## 8. Массовый запуск

Сделайте резервную копию Docker volume `remnawave_data`, затем измените:

```env
PAIRED_WHITELIST_MANAGE_ALL=true
PAIRED_WHITELIST_USER_SHORT_UUIDS=
```

Примените конфигурацию:

```bash
docker compose up -d --build
docker compose logs --tail=200 remnawave-traffic-limiter
```

Не удаляйте volume `remnawave_data`: в нём хранится связь основной и
технической учётных записей.

## 9. Обновление и откат

Перед обновлением сохраните текущий Git commit и резервную копию volume:

```bash
git rev-parse HEAD
docker compose ps
```

Обычное обновление limiter:

```bash
git pull
docker compose up -d --build
docker compose logs --tail=200 remnawave-traffic-limiter
```

Если новая версия не проходит пилот, вернитесь к заранее записанному commit,
пересоберите контейнер и не удаляйте SQLite volume. Не выполняйте сброс Git или
удаление volume без проверенной резервной копии.

## 10. Быстрый поиск неисправности

| Симптом | Что проверить |
| --- | --- |
| `502` на ссылке | Caddy на сервере подписок, локальный порт Subscription Page, правильность домена limiter. |
| `404` на `/api/state/*` извне | Это ожидаемо: маршрут намеренно закрыт. Проверяйте локально с diagnostics token. |
| `401` от `/api/pairing/*` | Совпадение `PAIRED_WHITELIST_LIMITER_TOKEN` и `PAIRED_WHITELIST_CONTROL_TOKEN`. |
| `501` на `/mihomo` или `/singbox` | Посмотрите, нет ли одинаковых тегов/имён узлов с разными параметрами в Main и WhiteList. |
| Технический пользователь не меняет лимит | Логи Bedolaga и limiter, доступность `PAIRED_WHITELIST_LIMITER_URL`, корректность UUID WhiteList. |

Для логов limiter:

```bash
docker compose logs -f remnawave-traffic-limiter
```
