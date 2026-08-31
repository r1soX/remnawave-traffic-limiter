# Режим Paired WhiteList

Paired WhiteList — режим Remnawave Traffic Limiter для раздельного учёта основного и WhiteList-трафика. Он использует API Remnawave и не зависит от Bedolaga. Сначала разверните базовую двухсерверную схему по [DEPLOYMENT.md](DEPLOYMENT.md), затем при необходимости подключите Bedolaga по [BEDOLAGA_PATCH_INSTALL.md](BEDOLAGA_PATCH_INSTALL.md).

## Поведение по типу тарифа

| Исходный пользователь Remnawave | Результат |
| --- | --- |
| Только Main, лимитированный | Ничего не меняется: Remnawave нативно учитывает трафик Main. |
| Main + WhiteList, безлимитный | Ничего не меняется: оба сквада остаются на одном пользователе. |
| Только WhiteList, лимитированный | Ничего не меняется: остаётся один пользователь с нативным учётом. |
| Main + WhiteList, лимитированный | Создаётся технический WhiteList-пользователь. Main становится безлимитным без WhiteList, а технический пользователь получает WhiteList и исходный лимит. |

Связь `main_user_id → white_user_id` хранится в SQLite volume сервиса. Когда пара не нужна, технический пользователь остаётся в панели, но переводится в `DISABLED` и исключается из выдаваемой подписки. При следующем подходящем тарифе он используется повторно.

`GET /api/state/<SHORT_UUID>` для активной пары показывает расход и общий лимит технического WhiteList-пользователя.

## Шлюз подписок

Шлюз работает по пути `/sub/<SHORT_UUID>`. Для активной пары он получает Main
и WhiteList из `SUBSCRIPTION_UPSTREAM_URL`, объединяет их в формате клиента и
передаёт в `Subscription-Userinfo` счётчик технического WhiteList-пользователя.

Поддерживаются URI/Base64, YAML форматов Mihomo/Clash/Stash и JSON форматов
sing-box/Xray. Без суффикса формат определяет Remnawave по User-Agent. Его
можно выбрать явно: `/mihomo`, `/clash`, `/stash`, `/singbox`, `/json` или
`/v2ray-json` после `SHORT_UUID`. Конфликтующие одноимённые узлы и
непрозрачные зашифрованные ответы не объединяются: вместо повреждённой
конфигурации сервис вернёт HTTP `501`.

Публичный домен подписок должен направлять клиентский путь в limiter по HTTPS,
а не в открытый Docker-порт. При внутреннем запросе к исходному сервису limiter
добавляет заголовок `X-Remnawave-Limiter-Gateway: 1`. Прокси пропускает его
напрямую к исходной Subscription Page, чтобы не возникла петля. Этот маршрут
обязательно ограничивается IP сервера limiter: внешнему клиенту нельзя давать
возможность подделать служебный заголовок.

Пример полного блока Caddy на сервере Subscription Page:

```caddy
SUBSCRIPTION_PUBLIC_DOMAIN {
    encode zstd gzip

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

    @subscription path_regexp subscription ^/([A-Za-z0-9_-]+)(/(json|v2ray-json|clash|singbox|mihomo|stash))?(&.*)?$
    handle @subscription {
        rewrite * /sub/{re.subscription.1}{re.subscription.2}
        reverse_proxy https://LIMITER_PUBLIC_DOMAIN {
            header_up Host LIMITER_PUBLIC_DOMAIN
        }
    }

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

В `.env` limiter укажите исходный публичный адрес подписок:

```env
SUBSCRIPTION_UPSTREAM_URL=https://SUBSCRIPTION_PUBLIC_DOMAIN
```

Не обслуживайте `/assets/*` статическим Caddy `file_server`: Subscription Page
выдаёт browser session-cookie, а `/assets/.app-config-v2.json` является
динамическим маршрутом. Limiter сохраняет этот cookie только в HTML-ответе для
браузера и не пересылает его при обновлении подписки VPN-клиентом.

## Пилотный запуск

1. Создайте резервную копию Docker volume `remnawave_data` и зафиксируйте у тестового пользователя сквады, лимит и дату окончания.
2. Проверьте нужный формат в целевом клиенте: обычный URI/Base64, `/mihomo`,
   `/clash`, `/stash`, `/singbox`, `/json` или `/v2ray-json`.
3. В `.env` limiter установите:

   ```env
   ENABLE_PAIRED_WHITELIST=true
   PAIRED_WHITELIST_USER_SHORT_UUIDS=SHORT_UUID_ТЕСТОВОГО_ПОЛЬЗОВАТЕЛЯ
   PAIRED_WHITELIST_MANAGE_ALL=false
   LIMIT_NOTICE_SQUAD_UUID=UUID_СКВАДА_ПОСЛЕ_ЛИМИТА
   SUBSCRIPTION_GATEWAY_ENABLED=true
   SUBSCRIPTION_UPSTREAM_URL=https://SUBSCRIPTION_PUBLIC_DOMAIN
   ```

4. Пересоберите limiter, настройте прокси и выполните сверку тестового пользователя. Сервис создаст одну техническую учётную запись с тегом `WL_LIMITER`.
5. Импортируйте исходную клиентскую ссылку в тестовое приложение. В ней должны быть Main и WhiteList-конфигурации, а отображаемый расход должен относиться к WhiteList-пакету. Повторите проверку для каждого используемого семейства клиентов.
6. Исчерпайте WhiteList-лимит и убедитесь, что технический пользователь получает сквад уведомления, а Main продолжает работать.

Не включайте массовую обработку, пока пилот не завершён. Не удаляйте SQLite volume, пока существуют активные технические пользователи: в нём хранится их связь с основными аккаунтами.

Для обработки всех подходящих пользователей установите:

```env
PAIRED_WHITELIST_MANAGE_ALL=true
```

Массовая обработка создаёт новые пары только для активных, неистёкших, лимитированных пользователей с Main и WhiteList.

## Изменения тарифа через Bedolaga

После создания, продления, смены тарифа, докупки трафика и смены статуса Bedolaga отправляет limiter полный целевой статус:

```text
POST /api/pairing/<SHORT_UUID>
X-Paired-Whitelist-Token: <PAIRED_WHITELIST_CONTROL_TOKEN>
Content-Type: application/json

{
  "enabled": true,
  "trafficLimitBytes": TRAFFIC_LIMIT_BYTES,
  "trafficLimitStrategy": "MONTH",
  "expireAt": "YYYY-MM-DDTHH:MM:SS+00:00",
  "status": "ACTIVE",
  "activeInternalSquads": ["UUID_MAIN", "UUID_WHITELIST"],
  "resetWhiteTraffic": false
}
```

Все поля обязательны. `enabled=true` означает лимитированный тариф с WhiteList: Main становится безлимитным без WhiteList, а технический пользователь получает лимит. `enabled=false` означает тариф только с Main, только с WhiteList или безлимитный тариф: основной пользователь получает переданные лимит, дату, статус и сквады, а технический пользователь отключается.

`resetWhiteTraffic=true` передаётся только при начале нового расчётного периода. Для смены тарифа и докупки трафика передаётся `false`: новый лимит применяется, но накопленный расход WhiteList не сбрасывается. Если расход уже больше нового лимита, WhiteList корректно переводится на сквад уведомления.

При `enabled=false` технический пользователь не удаляется и запись о паре остаётся в SQLite. Позднее `enabled=true` активирует ту же пару, не создавая нового пользователя.
