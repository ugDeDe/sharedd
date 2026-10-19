# 02. Контракты провайдеров

**Только план интеграции.** Внешние wire-форматы не смешивать с
[кабинетными DTO](03-cabinet-api.md). Источники проверены 16.10.2026:

- [Nodehost Developer API](https://nodehost.cc/api/v1/docs): таблицы и примеры
  находятся в JS `CONCEPTS`/`GROUPS` HTML, обычное text extraction их теряет.
- [Beget auth 1.2.1](https://developer.beget.com/ltd-beget/auth/v1.2.1/openapi.yaml).
- [Beget VPS 1.9.0](https://developer.beget.com/ltd-beget/vps/v1.9.0/openapi.yaml).
- [Принципы Beget.API хостинга](https://beget.com/ru/kb/api/obshhij-princzip-raboty-s-api).
- [getAccountInfo](https://beget.com/ru/kb/api/funkczii-upravleniya-akkauntom).

Существующий клиент `registry/internal/beget/client.go` не реализует все
перечисленные методы. Новый Nodehost adapter, IP lifecycle и balance adapters
предстоит написать. Не выводить поддержку из наличия endpoint в этой заметке.

## 1. Общие правила adapter

HTTPS с фиксированным allowlist host/base URL; пользователь не задаёт URL.
Context и размер response ограничены. Redirect с credentials на иной host
запрещён. Отладочные тела до редактирования секретов не логируются.
Нормализованный результат содержит outcome `confirmed|rejected|unknown`,
provider operation ID, resource ID, safe code и retry hint. HTTP 2xx сам по
себе не доказывает выполнение; проверить provider error и postcondition.

Read retry ограничен (до трёх попыток на один вызов, 1/2/4s с jitter,
deadline важнее). 429 учитывает `Retry-After`. Непонятный результат mutation
после отправки — unknown с сохранённым intent. Reconcile через чтение,
не повтор покупки. Даже HTTP DELETE не означает, что billing уже освобождён.

## 2. Nodehost

Base `https://nodehost.cc/api`; `Authorization: Bearer <secretref-resolved-key>`.
Ключи read-only тестового типа не являются sandbox платных мутаций.

| Метод и путь | Вход / scope | Успешный wire-результат |
|---|---|---|
| `GET /v1/account` | `account:read` | 200 `{id,email,username,balance,api:{test,mode,tier,maxServers,rateLimitRpm}}` |
| `GET /v1/catalog/locations` | `servers:read` | 200 `[{name,code,flag,priceMarkup}]` |
| `GET /v1/catalog/os-templates` | тот же | 200 `[{id,name,title?}]`; live: `title` отсутствует |
| `GET /v1/catalog/cpu-models` | optional query `location` | 200 массив строк |
| `GET /v1/catalog/disk-types` | — | 200 `[{id,name}]` (объекты, не строки) |
| `GET /v1/catalog/network-options` | required query `location` | 200 `{speeds:[...],hasAvailableNodes:true}` (live-verified 2026-10-17; докой обещанный `available` не приходит) |
| `GET /v1/catalog/limits` | — | 200 `{types:{<тип>:bool,...},limits:{cpu,ram,disk:{min,max}}}` |
| `POST /v1/pricing/estimate` | `servers:read`; JSON ниже | **201** (200 также принят) `{monthlyPrice,periodPrice,periodDays,geoMarkup,levelDiscountPct,apiDiscountPct,currency:"USD",...}` без покупки |
| `POST /v1/servers` | `servers:write`; стабильный `Idempotency-Key` | 202 `{operationId,status:"pending",type:"server.create",_links:{...}}` |
| `GET /v1/operations/{id}` | `servers:read` | 200 `{id,type,status,resourceType,resourceId,error}` |
| `GET /v1/servers` | `servers:read`; `limit=1..100`, default50; `cursor` | 200 `{data:[...],nextCursor:null\|string}` |
| `GET /v1/servers/{id}` | `servers:read` | 200 полная карточка, **содержит secret `password`** |
| `DELETE /v1/servers/{id}` | `servers:delete`, live key, API-created VPS | 200 `{message:"Server deleted",refund:0}`; затем подтвердить чтением |
| `GET /v1/servers/{id}/ip-subnets` | `servers:read` | 200 `[{rangeId,name,free}]` |
| `POST /v1/servers/{id}/change-ip` | `servers:write`; `{}` или `{subnetId:18}` | 200 `{ip,charged}`; платно, автоматически reboot |

**Live-verified 2026-10-17** — read-only вызовы тестовым ключом (ключ/аккаунт
нигде не фиксируются; мутации live не проверялись). Расхождения с докой:

1. `disk-types` — объекты `[{id,name}]`, не массив строк.
2. `limits` — `{types:{<тип>:bool},limits:{cpu,ram,disk:{min,max}}}`, не
   `{cores,ramGb,diskGb,types:[...]}`.
3. `os-templates` — записи `{id,name}`; поля `title` в live нет (optional).
4. `estimate` — успешный статус **201** (не 200; 200 также принимается);
   запрос — только `cores,ram,disk,networkSpeed,period,location`: передача
   `name/cpu/diskType/os` даёт 400 `property X should not exist` — эти поля
   принадлежат только create.

Остальное совпало с докой: account (balance decimal), locations
`{name,code,flag,priceMarkup}`, cpu-models (строки), servers `{data,nextCursor}`,
ошибка 401 `{error:{code,message}}` маппится по статусу.

Операции: `pending|running|succeeded|failed`; только succeeded с resourceId
позволяет перейти к чтению VPS. Failed может прийти после исходного 202,
включая недостаток средств. Поллинг — persisted `next_poll_at` (2s → 5s →
15s, максимум бюджет готовности 10min); timeout ожидания не означает провал
покупки. Поздний результат собирает reconciler.

Пример расчёта (не запрос на исполнение):

```json
{"cores":2,"ram":4,"disk":40,"networkSpeed":"1gbps","period":"1m","location":"DE"}
```

Пример заказа:

```json
{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m","networkSpeed":"1gbps"}
```

Значения сверяются с актуальным каталогом, не с примером. CPU/cores/ram/disk,
diskType/os/location/name обязательны; period/networkSpeed/promoCode optional.
Документированные optional `domain` и `password` не принимать как открытый
графовый secret: root password генерирует провайдер, полученный пароль
помещается в encrypted credential store и выдаётся worker только по secretref.
Name до 100 символов, численные размеры — целые, диапазоны из limits.
Периоды `1d|3d|1w|1m|3m|6m|1y`. RAM/disk в GB, не Beget MB.

Создание поддерживает повтор с тем же provider Idempotency-Key: тот же ответ,
а пока выполняется — 409. Key фиксируется до первого I/O и никогда не меняется
при retry/restart; неизвестный срок хранения provider key не позволяет считать
его вечной гарантией. После длительного unknown сначала reconcile списка/
operations. Смена IP не имеет документированной идемпотентности: не повторять
POST при потере ответа. Прочитать текущий IP и сопоставить с сохранённым старым;
если нельзя доказать один результат — needs_attention.

По документации случайный change-ip стоит $0.30, выбор subnet $0.50;
это справочные цены на дату сверки, перед confirm показывать источник и
дату, не обещать неизменность тарифа. Snapshot баланса — `balance` из
`/v1/account`, USD, scope `nodehost_api`: API-баланс отдельный от баланса сайта.
Платёжные/topup endpoints не включать в allowlist блоков.

Ошибка провайдера: `{error:{code,message,details?},requestId}`.

| HTTP | Действие |
|---|---|
| 400 | validation failed, исправление входа, без retry покупки |
| 402 | `insufficient_funds`, pause, без fallback purchase |
| 403 | проверить scope/лимит/test key; не считать недостатком средств |
| 404 | чтение: отсутствие; удаление: отсутствие подтверждать в том же connection |
| 409 | create с тем же key: reconcile/poll; иной conflict не подменять успехом |
| 429 | backoff + Retry-After; не создавать новый key |
| 5xx / timeout после mutation | unknown, read reconciliation |

Провайдерский `requestId` — только внутренний диагностический атрибут после
проверки формата; карточка кабинета не получает сырые провайдерские ответы.

## 3. Beget cloud authentication и VPS

Base `https://api.beget.com`. `POST /v1/auth` с
`{"login":"...","password":"...","saveMe":true}` → 200 `{token}` либо
200 `{error:"CODE_REQUIRED_TOTP"}` и другие документированные ошибки.
`code` добавляется только для активного 2FA, без 2FA поле отсутствует.
Коды `CODE_REQUIRED_EMAIL|CODE_REQUIRED_SMS|CODE_REQUIRED_TOTP` показываются
как требование ввода кода, сам код не сохраняется. JWT хранится зашифрованно,
последующие запросы — Bearer. `POST /v1/auth/refresh` без тела с текущим Bearer
→ 200 `{token}` либо `{error}`. Refresh/reauth коалесцируются по revision.
После 401 разрешён ограниченный повтор чтения; purchase POST автоматически
после reauth не повторяется. Background run не может сам угадать 2FA.

| Метод и путь | Запрос | Ответ и postcondition |
|---|---|---|
| `GET /v1/vps/region` | — | 200 `{regions:[{id,name,available,...}]}` |
| `GET /v1/vps/configuration` | — | 200 `{configurations:[...],...}`; сохранить provider ID и единицы |
| `GET /v1/vps/configurator/calculation` | query `params.cpu_count`, `params.disk_size`, `params.memory`, `region`; optional `configuration_group` | 200 `{success:{settings,params,price_day,price_month,price_hour}}` либо `{error:{code,message}}`; без заказа |
| `POST /v1/vps/server` | JSON ниже | 200 `{vps:{id,...}}` либо `{error:{code,message}}` |
| `GET /v1/vps/server/list` | `limit`, `offset`; optional `filter`, `sort` | 200 `{vps:[...],total_count}` |
| `GET /v1/vps/server/{id}` | UUID | 200 `{vps:{id,status,ip_address,...}}`; parse business errors |
| `POST /v1/vps/network` | `{"id":"<vps_uuid>","address_family":"IP_V4"}` | 200 `{additional_ip:{ip,vps_id,region}}` либо error_v2/error |
| `GET /v1/vps/network` | optional filter | 200 `{ip:[...],additional_ip:[{ip,vps_id,region,...}],private_network:[...]}`; проверить primary и additional inventory |
| `POST /v1/vps/{id}/network/{ip_address}` | path UUID/IP, без тела | 200 `{vps:{...}}` либо `{error:{code,message}}`; прочитать привязку |
| `DELETE /v1/vps/network/detach/{ip_address}` | без тела | 200 `{vps:{...}}` либо `{error:{code,message}}`; **detach не освобождает платный адрес** |
| `DELETE /v1/vps/network/{ip_address}` | без тела | 200 `{ip_address}` либо `{error:{code,message}}`; подтвердить отсутствие в network |
| `POST /v1/vps/server/{id}/remove` | `{"ip_action":"DELETE"}` | 200 `{vps:{...}}` либо `{error:{code,message}}`; проверить отсутствие VPS и управляемых IP |

Пример создания по готовому тарифу:

```json
{"display_name":"pool-node","hostname":"pool-node","description":"sharedd operation marker","configuration_id":"<catalog-id>","image_id":"<image-id>","ssh_keys":[123],"region":"ru1"}
```

Вместо `configuration_id` допускается `configuration_params` с
`cpu_count`, `memory`, `disk_size` из конфигуратора; memory/disk_size в MB.
Два способа одновременно запрещены внутренней валидацией. SSH key должен
принадлежать connection; существующий adapter умеет `GET/POST /v1/vps/sshKey`.
Нельзя добавлять новый ключ повторно после неизвестного результата без сверки.
Примеры ID символические, не захардкоженные production параметры.

Для network order современный успешный результат `additional_ip` приоритетен;
deprecated `ip_address` принимается только как совместимость. Проверять
`error_v2` и deprecated `error` даже при HTTP200. Противоречие success/error,
пустой IP, иной `vps_id`/region — не успех, а ошибка контракта/unknown с
reconcile. У attach/detach/remove также разбирать business errors.

Удаление VPS должно явно выбрать `ip_action:DELETE`; `RELEASE` оставляет
ресурсы аккаунту и не соответствует требованию освободить старые IP.
Если у VPS есть чужие/неуправляемые адреса, общий DELETE не отправлять:
409 `unmanaged_ip_attached`, ручное разрешение вопроса, а не удаление чужого IP.

Нет документированного endpoint promotion дополнительного IP в primary.
Поэтому order/attach нового IPv4 ещё не доказывает возможность удалить старый
primary. Это отдельный integration gate; до подтверждения автоматическая
ветка должна остановиться на `needs_attention`, сохраняя старую сеть.

### 3.1 Неопределённые мутации

Beget purchase/order не имеет документированного Idempotency-Key.
Сохранить intent и маркер заказа до POST; запрос отправить один раз.
После потери ответа сверять inventory до/после и unique marker VPS. Для IP
сравнивать адреса, family, region и binding. Ноль совпадений при eventual
consistency не доказывает отказ; несколько совпадений — ручной checkpoint.
Нельзя «на всякий случай» повторить покупку или переключиться на VPS fallback.
Подтверждённый неплатёжный отказ IP swap может разрешить fallback только
в явно выбранном PROFILE; manual-команда никогда этого не делает.

## 4. Beget balance: отдельный API хостинга

Использовать **POST** `https://api.beget.com/api/user/getAccountInfo`:

```text
Content-Type: application/x-www-form-urlencoded

login=<urlencoded-login>&passwd=<urlencoded-api-password>&output_format=json
```

POST прямо разрешён и рекомендован страницей принципов API. Пароль API
может отличаться от пароля cloud JWT; хранить `hosting_api_login`/
`hosting_api_password` отдельно. Не переносить credentials в query URL,
access logs, traces, graph literals или shell. API-доступ может требовать
включения и allowlist исходящего IP в панели Beget.

Успех имеет два уровня:

```json
{"status":"success","answer":{"status":"success","result":{"user_balance":1339.57}}}
```

Проверить оба `status`, затем наличие конечного десятичного `user_balance`.
`status:error` содержит `error_code/error_text`; внутренний
`answer.status:error` — `answer.errors[]`. Пустое/нечисловое значение —
ошибка, не 0. JSON number читать десятично, не через float64 с округлением.

Этот источник документирован для **виртуального хостинга**, не cloud billing.
Нормализовать как scope `beget_hosting_unverified`, `currency:null`;
VPS-only поддержка и соответствие cloud расходам **UNCONFIRMED**.
До отдельного подтверждения показывать «Баланс API хостинга; применимость
к VPS не подтверждена». При отсутствии этих credentials возвращать
unavailable с `balance_credentials_missing`, а не брать cloud password.

## 5. Balance DTO и refresh

```json
{"amount":"142.50","currency":"USD","scope":"nodehost_api","freshness":"fresh","checked_at":"2026-10-16T10:00:00Z","last_attempt_at":"2026-10-16T10:00:00Z","error":null}
```

`amount` — string decimal или null только если ещё нет хорошего снимка;
отрицательное документированное значение допустимо. Currency string ISO-код
только от известного источника, иначе null. Freshness `fresh|stale|unavailable`:
fresh пока успех не старше 60s и последняя попытка не провалилась; ошибка
с имеющимся снимком немедленно делает stale. `checked_at` — только последний
успех, `last_attempt_at` — последняя попытка; null если её не было.
`error` null либо `{code,message}` без provider raw body/PII.

Coalesce один refresh на `(owner,connection,credential_revision)`, throttle
одна внешняя попытка за 60s, timeout 15s. Повторный UI запрос в окне получает
200 текущий снимок (включая last error), не новое внешнее обращение. Общий
кабинетный rate limit всё равно может дать 429. На ошибке provider refresh
возвращает 200 snapshot со stale/unavailable; это не ошибка изменения
connection. Смена credentials увеличивает revision и делает старый баланс
непригодным для отображения; новый refresh начинает отдельный namespace.
Запись результата — CAS по revision. Не суммировать даже одинаковые валюты
с неизвестной областью списаний; не авторизовывать покупку локальным балансом.
