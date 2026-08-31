# Remnawave Traffic Limiter

Сервис для раздельного учёта трафика в Remnawave. Он подходит для тарифов, где основной доступ должен работать независимо, а трафик специальных WhiteList/LTE-ресурсов ограничен отдельным пакетом.

## Зачем он нужен

Обычный пользователь Remnawave считает весь трафик в одном счётчике. Из-за этого обычный доступ может израсходовать пакет, который предназначен только для WhiteList/LTE.

В режиме **Paired WhiteList** сервис создаёт одну служебную пару для подходящего тарифа:

- основной аккаунт обслуживает Main-доступ без лимита;
- технический аккаунт обслуживает только WhiteList и хранит лимит тарифа;
- клиент по-прежнему использует одну обычную ссылку подписки;
- в приложении отображается расход WhiteList-пакета;
- после исчерпания пакета отключается только WhiteList, Main продолжает работать.

Технический аккаунт не является второй подпиской клиента. Он нужен только для точного учёта и скрыт за обычной ссылкой пользователя.

## Правила для тарифов

| Тариф | Результат |
| --- | --- |
| Только Main с лимитом | Один обычный пользователь, нативный учёт Remnawave. |
| Только WhiteList с лимитом | Один обычный пользователь, без пары. |
| Main + WhiteList без лимита | Один обычный пользователь, без пары. |
| Main + WhiteList с лимитом | Одна основная и одна техническая WhiteList-учётная запись. |

При смене тарифа новые технические аккаунты не создаются. Ненужная пара переводится в `DISABLED`, а при возврате на подходящий тариф переиспользуется.

## Требования

- Remnawave с доступным API пользователей и webhook-ами;
- Docker Engine и Docker Compose v2;
- UUID внутренних сквадов Main, WhiteList и страницы/сквада после исчерпания лимита;
- API-токен Remnawave с правами чтения и изменения пользователей;
- публичный домен подписок, который можно настроить через обратный прокси.

Для связки с Bedolaga используйте [инструкцию по установке патча](BEDOLAGA_PATCH_INSTALL.md).

## Установка

### 1. Подготовьте проект

```bash
git clone <URL_РЕПОЗИТОРИЯ> remnawave-traffic-limiter
cd remnawave-traffic-limiter
cp .env.example .env
nano .env
```

### 2. Заполните `.env`

Минимальная рабочая конфигурация:

```env
REMNAWAVE_PANEL_URL=https://panel.example.com
REMNAWAVE_API_TOKEN=...
WEBHOOK_SECRET=...
DIAGNOSTICS_TOKEN=ОТДЕЛЬНЫЙ_СЛУЧАЙНЫЙ_СЕКРЕТ

BASIC_SQUAD_UUID=UUID_MAIN
WHITELIST_SQUAD_UUID=UUID_WHITELIST
LIMIT_NOTICE_SQUAD_UUID=UUID_LIMIT_NOTICE

ENABLE_PAIRED_WHITELIST=true
PAIRED_WHITELIST_USER_SHORT_UUIDS=SHORT_UUID_ТЕСТОВОГО_ПОЛЬЗОВАТЕЛЯ
PAIRED_WHITELIST_MANAGE_ALL=false

PAIRED_WHITELIST_CONTROL_TOKEN=ОТДЕЛЬНЫЙ_СЛУЧАЙНЫЙ_СЕКРЕТ

SUBSCRIPTION_GATEWAY_ENABLED=true
SUBSCRIPTION_UPSTREAM_URL=https://sub.example.com
```

Сгенерировать секрет:

```bash
openssl rand -hex 32
```

`PAIRED_WHITELIST_CONTROL_TOKEN` должен быть отдельным от `WEBHOOK_SECRET`.
`DIAGNOSTICS_TOKEN` также должен быть отдельным секретом.

### 3. Запустите сервис

```bash
docker compose up -d --build
docker compose logs -f remnawave-traffic-limiter
```

Проверка:

```bash
curl http://127.0.0.1:8080/health
# {"status":"ok"}
```

## Настройка Remnawave

Для production не публикуйте Docker-порт limiter наружу. Оставьте
`BIND_ADDRESS=127.0.0.1` и поставьте HTTPS reverse proxy. Готовый шаблон для
Caddy: [Caddyfile.production.example](Caddyfile.production.example).

Внешнему миру нужны только три пути: `/webhook`, `/sub/*` и
`/api/pairing/*`. Пути `/api/state/*`, `/api/reconcile/*`, `/health` и
`/ready` не публикуются: ими пользуются только локально через SSH. Внешний
запрос к `/api/state/test` должен возвращать `404`, а не `401` и не JSON.

Если Caddy работает в контейнере, после изменения конфигурации проверьте и
перезагрузите его без остановки прокси:

```bash
docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

В шаблоне указан порт `8080`, потому что это значение по умолчанию. Если в
`.env` задан другой `PORT`, в `reverse_proxy` Caddy должен быть указан тот же
локальный порт.

После настройки DNS и Caddy включите webhook в панели и задайте адрес:

```text
https://LIMITER_PUBLIC_DOMAIN/webhook
```

Секрет в панели должен совпадать с `WEBHOOK_SECRET`. Сервис принимает HMAC-SHA256 подписи в заголовках `X-Remnawave-Signature`, `X-Remnawave-Signature-256`, `X-Signature` и `X-Hub-Signature-256`.

Webhook обрабатывает изменения сразу. Фоновая сверка раз в `POLL_INTERVAL` (по умолчанию 45 секунд) восстанавливает состояние, если событие не дошло.

## Настройка ссылки подписки

В парном режиме публичная ссылка должна направляться в сервис по пути `/sub/{shortUuid}`. Он получает исходную подписку из `SUBSCRIPTION_UPSTREAM_URL`, объединяет Main и WhiteList-конфигурации и возвращает клиенту один ответ.

Пример Caddy. Замените адреса на свои:

```caddy
@limiter_upstream header X-Remnawave-Limiter-Gateway 1
handle @limiter_upstream {
    reverse_proxy 127.0.0.1:3010
}

@subscription path_regexp subscription ^/([A-Za-z0-9_-]+)$
handle @subscription {
    rewrite * /sub/{re.subscription.1}
    reverse_proxy LIMITER_HOST:8080
}
```

Шлюз объединяет URI-списки и Base64-кодированные URI-списки. Непрозрачные форматы, такие как HAPP, Clash YAML или sing-box JSON, намеренно не объединяются: сервис вернёт `501`, а не повреждённую конфигурацию.

## Пилот и запуск на всех пользователей

Сначала настройте одного тестового пользователя:

1. Оставьте `PAIRED_WHITELIST_MANAGE_ALL=false`.
2. Впишите его `shortUuid` в `PAIRED_WHITELIST_USER_SHORT_UUIDS`.
3. Проверьте подписку в клиенте, расход WhiteList и реакцию после лимита.
4. Сделайте резервную копию Docker volume `remnawave_data`.

Для массового запуска установите:

```env
PAIRED_WHITELIST_MANAGE_ALL=true
```

и пересоберите контейнер:

```bash
docker compose up -d --build
```

Новые пары создаются только для активных лимитированных пользователей, у которых есть и Main, и WhiteList. Тарифы только с Main, только с WhiteList и безлимитные тарифы остаются нативными.

## Интеграция Bedolaga

После покупки, продления, смены тарифа и докупки трафика Bedolaga отправляет сервису полный целевой статус. Поэтому лимит, дата окончания и сквады меняются сразу, без ожидания фоновой сверки.

На стороне Bedolaga используйте тот же публичный HTTPS-домен reverse proxy:

```env
PAIRED_WHITELIST_LIMITER_URL=https://LIMITER_PUBLIC_DOMAIN
PAIRED_WHITELIST_SQUAD_UUID=UUID_WHITELIST
PAIRED_WHITELIST_LIMITER_TOKEN=ТОТ_ЖЕ_PAIRED_WHITELIST_CONTROL_TOKEN
```

Подробная установка и повторное применение после `git pull`: [BEDOLAGA_PATCH_INSTALL.md](BEDOLAGA_PATCH_INSTALL.md).

## API и диагностика

| Метод | Путь | Назначение |
| --- | --- | --- |
| `GET` | `/health` | Проверка, что процесс жив. |
| `GET` | `/ready` | Проверка готовности. |
| `POST` | `/webhook` | Приём webhook Remnawave. |
| `GET` | `/api/state/{shortUuid}` | Состояние и счётчик пользователя. Для пары — WhiteList-счётчик. |
| `POST` | `/api/reconcile/{shortUuid}` | Сверка одного пользователя. |
| `POST` | `/api/pairing/{shortUuid}` | Защищённый endpoint для Bedolaga. |
| `GET` | `/sub/{shortUuid}` | Объединённая подписка, если шлюз включён. |

Примеры:

```bash
curl -sS \
  -H "X-Limiter-Diagnostics-Token: $DIAGNOSTICS_TOKEN" \
  http://127.0.0.1:8080/api/state/USER_SHORT_UUID

curl -X POST \
  -H "X-Limiter-Diagnostics-Token: $DIAGNOSTICS_TOKEN" \
  http://127.0.0.1:8080/api/reconcile/USER_SHORT_UUID
```

`/api/state` и `/api/reconcile` требуют заголовок `X-Limiter-Diagnostics-Token` и ограничены 60 запросами в минуту на источник. `/api/pairing` требует `X-Paired-Whitelist-Token`.

Пример production-проверки безопасности с другой машины:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://LIMITER_PUBLIC_DOMAIN/api/state/test
# 404
```

## Эксплуатация

```bash
# Последние логи
docker compose logs --tail=200 remnawave-traffic-limiter

# Просмотр логов
docker compose logs -f remnawave-traffic-limiter

# Остановка без удаления данных
docker compose down

# Обновление
git pull
docker compose up -d --build
```

SQLite-состояние хранится в Docker volume `remnawave_data`. Не удаляйте volume, пока в Remnawave существуют технические пользователи: в нём хранится связь между основными и техническими аккаунтами.

## Разработка

Требуется Go 1.22+.

```bash
go test ./...
go vet ./...
go build -o bin/remnawave-traffic-limiter ./cmd/server
```

Дополнительно доступны `make test`, `make lint`, `make build`, `make docker-up` и `make docker-logs`.

## Дополнительная документация

- [PAIRED_WHITELIST.md](PAIRED_WHITELIST.md) — детальная логика пар и смены тарифов.
- [BEDOLAGA_PATCH_INSTALL.md](BEDOLAGA_PATCH_INSTALL.md) — интеграция Bedolaga.

## Безопасность

- Не публикуйте `.env`, токены и секреты.
- Используйте разные сильные секреты для webhook и Bedolaga control endpoint.
- В production оставляйте `BIND_ADDRESS=127.0.0.1`; внешний доступ предоставляйте только через HTTPS reverse proxy. Шаблон Caddy публикует webhook, подписки и endpoint Bedolaga, но не диагностику.
- Перед массовой миграцией резервируйте Docker volume и проверяйте все типы тарифов на тестовом пользователе.
