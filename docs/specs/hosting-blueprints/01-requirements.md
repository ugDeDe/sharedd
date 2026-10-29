# 01. Требования и инварианты

**Только план.** Связанные контракты: [провайдеры](02-provider-api.md),
[кабинет](03-cabinet-api.md), [исполнение](04-storage-and-execution.md),
[графы](05-blueprint-profiles.md), [приёмка](06-implementation-and-verification.md).

## 1. Текущее состояние и целевое изменение

| Сейчас, по исходникам | Требуется |
|---|---|
| Beget-связка одна на пользователя; есть заказ VPS и SSH-профили | Несколько owner-scoped connections, Nodehost и Beget, полный жизненный цикл |
| Профиль — текстовый playbook, операция хранит `profile_id`; при исполнении/возобновлении выбирается последняя версия | Новые операции закрепляют `profile_version_id` и входы; старые операции не переписываются |
| `globalpingLoop` пропускает цикл при `metricsMuted()` / `banActive()`; `selfTerminate` блокирует вызвавшего в recovery и затем завершает процесс | Health-состояние не останавливает планировщик GP; recovery неблокирующий и не требует рестарта |
| Первая регистрация вызывается до старта фоновых циклов | Даже её отказ/terminate не мешает старту GP |
| SSH handshake 20 секунд; provisioning передаёт настроечный `StepTimeout`, default `install_timeout_min=20` | Отдельное обнаружение потери транспорта около 60 секунд; отсутствие stdout не является потерей связи |
| В рабочем `sshx.Run` запасной timeout при неположительном аргументе 15 минут | Не путать этот fallback с конфигурационным default шага 20 минут; бюджет всегда передавать явно |

## 2. SSH и завершение установки

1. TCP connect и SSH handshake ограничены 20 секундами каждый, но общий
   context операции/шага может завершить их раньше. Ограничение шага по
   конфигурации по умолчанию 20 минут; оно не продлевается выводом команды.
2. После handshake транспорт отправляет keepalive с ожиданием ответа.
   Целевой бюджет обнаружения blackhole — около 60 секунд: интервал 20 секунд,
   отсутствие ответа 60 секунд с последнего подтверждения завершает транспорт.
   Отрицательный ответ на неизвестный SSH request подтверждает живой транспорт;
   не считать его потерей связи. Конкретный request и тестовый clock фиксируются
   при реализации. Заблокированный `SendRequest` прерывается закрытием socket.
3. Тихая команда, не пишущая stdout/stderr несколько минут, продолжает работу,
   если транспорт отвечает. Не вводить stdout-idle timeout вместо keepalive.
4. Cancel/deadline закрывает socket и session и разблокирует dial, handshake,
   `NewSession`, отправку команды/stdin, ожидание результата и output readers.
   Не оставлять goroutine, владеющую lock, после возврата handler/worker.
5. Финализировать step (`error` с причиной `ssh_transport_lost`, `timeout`
   или `cancelled`) и operation, записать время и безопасный хвост вывода;
   освободить execution lease/локальный lock. Резерв денег/ресурса не исчезает
   от освобождения execution lock. Если команда могла выполниться, сохранить
   `needs_attention` checkpoint с неопределённым результатом.
6. Не переигрывать SSH вслепую после обрыва/restart. Только блок с явной
   доказуемой идемпотентностью и postcondition может повториться автоматически.
   Для произвольного shell нужен ручной checkpoint; cancel не откатывает
   удалённые изменения автоматически.

## 3. Globalping: обязательный независимый цикл

### 3.1 Жизненный цикл

Планировщик GP стартует независимо от результата initial register и fetch
config. `ip_ban`, `dead`, prune/quarantine, плохие метрики и отказ отчёта
не завершают его и не блокируют вызывающие циклы. `selfTerminate` должен
переводить recovery-состояние и возвращать управление, а не ждать IP внутри
`sync.Once.Do`. Дедуплицируется переход состояния, а не пожизненная попытка
восстановления; повторные циклы ban → recovery поддерживаются без лимита.

«GP никогда не останавливается» означает живой scheduler и конечную попытку
по расписанию, а не создание заведомо невалидного measurement без адреса,
конфига или слушающего порта. Каждая попытка имеет deadline; при отсутствии
нужных данных/GP API outage записывает причину и возвращается к следующему
тику. Ожидание локального TCP конечное (сейчас `gpProxyWaitTimeout=75s`).
Не держать бесконечный poll внутри одного прогона. Метрики могут мешать
допуску, но не запуску следующей попытки GP.

### 3.2 Same-IP recovery

Последовательность для одной identity:

1. Получить адрес: explicit public IP при управляемой смене, иначе resolver.
2. Выполнить LOCAL GP до завершённого результата, сохранить measurement ID,
   target/port, время, ratio и локальный итог. Сам факт POST создания
   measurement или старый кэш не является успешным завершением.
3. Если локальный результат успешен, запланировать повторную регистрацию
   **с того же IP**, не требуя его смены или рестарта. Сохранить текущий
   `node_id`, personal secret, owner/pending claim и server Bearer auth.
4. Передать evidence в расширении machine API, определённом ниже; не
   заменять им аутентификацию. Ввести расширение DTO и contract tests
   одновременно с registry и agent.
5. Registry сам GET-проверяет measurement через существующий
   `internal/globalping`: binding target, port, тип, время/свежесть,
   завершённость и успешность. Заявление агента не снимает бан само по себе.
6. Допуск только после применимых cooldown/reverify правил и свежих metrics/TCP.
   `dead` допускает восстановление по существующему правилу re-register;
   `ip_ban` сохраняет независимую проверку и cooldown. Владение не исчезает
   при prune; новая VPS получает новую identity, смена IP той же VPS — старую.

Регистрация получает общий backoff gate на identity/IP: `Retry-After` для
429 имеет приоритет; сетевые ошибки — ограниченный exponential backoff с
jitter (начало 5s, максимум 5min), без busy loop. GP продолжает расписание
во время backoff; хранится только последний пригодный evidence, просроченный
не используется. Один completed measurement вызывает не более одной
логической recovery registration; повторы HTTP ограничены gate.

Успешная recovery registration не должна снова запускать GP тем же evidence.
Различать причину регистрации и поколение measurement: initial admit может
дать один coalesced kick, recovery от GP — без обратного kick. Параллельные
heartbeat/metrics/GP terminate-сигналы не создают register → kickGP шторм.
401 не превращается в обход Bearer/personal-secret проверки; исправление
секретов идёт существующим аутентифицированным путём, без их ротации от бана.

### 3.3 Предлагаемое расширение POST /register

Это расширение существующего machine endpoint, не кабинетный маршрут.
Текущий request имеет `node_id`, `ip`, optional `node_type` и `authkey`.
Добавить optional `recovery_evidence`:

```json
{"node_id":"pool-abc12","ip":"203.0.113.10","node_type":"classic","recovery_evidence":{"globalping_measurement_id":"measurement-example","ip":"203.0.113.10","port":443,"completed_at":"2026-10-16T10:00:00Z"}}
```

Адрес примера документационный, реальная валидация публичного IP сохраняется.
Measurement ID — непустая строка до128 ASCII символов `[A-Za-z0-9_-]`,
port integer1..65535 и равен shared proxy port, ip совпадает с request ip,
completed_at RFC3339 UTC; claimed timestamp не заменяет время из GP API.
Не принимать bool `admitted`/`verified` от агента. Разбирать только известные
поля evidence и ограничить всё тело действующим machine body limit.

Порядок: существующий Bearer gate → identity/pending/owner authorization →
durable retirement fence → bounded independent measurement GET → cooldown/
admission decision. Для неизвестной ноды evidence не даёт владения. Блокирующее
GP I/O вынести из глобальных/claim locks; перед commit повторно CAS-проверить
identity, owner, fence и ban episode. Нет session/CSRF и нет необходимости
нового command Idempotency-Key: повтор своей регистрации должен быть
идемпотентен по identity/evidence, без расхода второго quota slot.

Успех HTTP200 сохраняет существующий response `{}` либо
`{"node_secret":"<issued-secret>"}` при штатной выдаче секрета. Re-register
с собственным personal/pending secret не ротирует его, как и текущий gate.
Не расширять этот ответ несовместимой кабинетной envelope.

| HTTP | Предлагаемая recovery-семантика |
|---|---|
| 400 | Невалидный request/evidence; сохраняется machine error contract |
| 401 | Нет действующей server/node/pending authentication; не обходить новым evidence |
| 403 | Существующий отказ owner/disabled либо terminate JSON `{terminate:true,reason:"ip_ban",message}` при доказанно отрицательной проверке; retirement возвращает отдельный reason `superseded`/`deleting` |
| 429 | Cooldown/prune/reverify ещё не готовы; `Retry-After` и существующие `error`, `retry_after_sec`; GP scheduler не останавливать |
| 503 | Control unavailable, fail-closed, либо временно недоступен verification service; backoff, не admission |

При временной GP outage не фиксировать окончательный failed reverify по
одному транспортному сбою; вернуться по backoff со свежим evidence. Старые
агенты без поля продолжают действующий registry reverify-путь; новый
evidence ускоряет проверку завершённого локального measurement, не отменяет
проверки. `/report` по-прежнему несёт `globalping_measurement_id`; отказ report
не должен помешать запланированной recovery registration.

### 3.4 Fence удаления

Health ban не равен retirement. Для заменённой/удаляемой VPS registry хранит
отдельный durable fence identity/resource generation: `superseded` либо
`deleting`. Он запрещает reclaim/admission старой ноды даже после успешного
GP и prune. GP на физически существующем старом хосте может продолжаться;
registry отвечает конечным отказом и агент не штурмует register. Fence
проверяется также в report/heartbeat и выдаче shared config. Он не снимается
обычным health recovery; отмена замены — отдельное разрешённое решение с CAS.

## 4. MANUAL и PROFILE

**MANUAL — default для новых и всех мигрированных нод.** Бан любого вида
вызывает ровно ноль provider mutations. Агентское recovery/GP продолжается.
В настройках ноды доступны явные команды создать VPS, удалить, сменить IP,
заменить. Каждая платная/разрушительная команда требует подтверждения и
идемпотентности по [API](03-cabinet-api.md).

Ручная смена IP не имеет fallback на создание VPS: при отказе, unknown,
недостатке средств или неподдерживаемой функции пользователь получает
состояние операции, а не неожиданную покупку.

PROFILE включается только с видимой пользователю immutable версией и
owner-scoped ссылками inputs. Автодействия ограничены возможностями этой
версии, связанной VPS и бюджетообразующими лимитами. При manual → profile
`process_current_ban` по умолчанию false: ждать следующего нового эпизода,
а не тратить деньги за накопленную историю.

При profile → manual одной SQLite-транзакцией увеличить revision и
инвалидировать разрешения будущих paid/destructive dispatch. Worker проверяет
revision непосредственно перед отправкой. Если dispatch уже захвачен и
отправка началась, запрос считается in-flight; его исход согласуется через
reconcile, без автоматического rollback/delete. Победитель гонки определяется
CAS записи dispatch, а не временем возврата HTTP. Новый профиль/inputs также
инвалидирует старое поколение; старый run не получает новые полномочия.
`expected_revision` обязателен; несовпадение — 409, чужая нода — 404.

## 5. Замена и допуск

- Replacement сначала создаётся и устанавливается, затем независимо
  допускается в пул; ожидание мастерства/DNS assignment запрещено.
- Fresh GP, метрики, локальный/registry TCP и shared proxy port проверяются
  действующими правилами. Новая VPS с тем же забаненным IP не годится.
- После допуска: fence старой identity → атомарно переключить current resource
  и management policy → удалить старую VPS → подтвердить отсутствие → наблюдать
  новую. Сбой cleanup не запускает новую покупку.
- Same VPS/IP swap: сохранить identity и секреты; новый advertised address
  проверить до удаления старого. Удалять только адреса, которыми управляет
  данный run и на удаление которых есть provenance/согласие.
- Недостаток средств при попытке Beget IP swap — пауза, не бессмысленный
  fallback create. Unknown заказа IP сначала reconciliate, иначе может
  появиться и лишний IP, и лишняя VPS.
- Квота допускает одну связанную временную replacement при полной квоте;
  это не общий дополнительный слот и не способ делать цепочку покупок.

## 6. Баланс и изоляция

Показывать отдельно по connection: decimal string `amount`, nullable
`currency`, `scope`, `freshness`, время последнего успеха и попытки,
санитизированную ошибку. TTL 60s. Ошибка сохраняет последний хороший снимок
как stale; без него unavailable, никогда фиктивный ноль. Валюты не суммируются.
Нет пополнений. Даже fresh balance — справка, а не разрешение покупки;
списание и окончательное решение — только у provider API.

Объединять одновременные refresh, ограничивать частоту и context. Ключ кэша
включает owner, connection и credential revision. Запоздавший ответ старых
credentials не может затереть новый снимок. Браузер/граф/лог не получают
пароли, JWT, API key, root password или приватный SSH key.
