# 05. Графовый редактор и готовые профили

**Нормативный будущий формат v1. Исполнителя этого формата сейчас нет.**
Ниже полные шаблоны графов в Markdown; их нельзя выдавать за executable
profile files или импортировать текущим текстовым runner.
API — [03](03-cabinet-api.md), checkpoints — [04](04-storage-and-execution.md).

## 1. Возможности и ограничения редактора

Редактор покрывает полный жизненный цикл VPS, а не только shell steps:
каталог/config → estimate → create → provider wait → SSH/install → pool
admission → durable ban wait → branch IP swap/replacement → cleanup.
Администратор создаёт и публикует версии; пользователь выбирает опубликованную,
создаёт частную либо копирует видимую. Редактирование создаёт новую версию;
активный run остаётся на своей.

Текущий UI — vanilla TypeScript, Go-esbuild через `tools/uigen`; миграция на
Vue/React/npm не входит в план. Решение о framework-free graph library
принимается после небольшого прототипа: совместимость с текущим bundler,
лицензия, keyboard editing, typed ports, размер bundle. Никакая библиотека
в этом документе не объявляется уже установленной. Native SVG/DOM допустимы,
если покрывают требования без отдельного canvas framework.

Обязательный процесс будущих UI работ: **design → frontend implementation
→ designer QA**, с critique/audit/polish, описанием tokens/states/a11y в
design artifacts. Это требование реализации, не выполненная в этой
документальной поставке дизайнерская проверка.

Интерфейс: palette только разрешённых blocks; inspector строгих params;
разные control/data ports; список диагностик с focus на узле; run view
checkpoints/веток/стоимости/unknown; явные confirm и mode switch. Баланс
показывает scope/stale/error, не даёт ложного разрешения тратить.
Нужны loading/error/empty, keyboard add/connect/remove, undo локальной правки,
подписи портов для screen reader, достаточный contrast, responsive inspector.
Нельзя делать управление доступным только drag-and-drop/цветом.

В v1 нет произвольных циклов, параллельных loops, subgraphs, пользовательского
кода в expression и generic arbitrary HTTP. Ограниченные retry/wait —
семантика блока, не back-edge графа. Наблюдение за новым ресурсом запускает
новое поколение run через scheduler, не цикл и не рекурсивный subgraph.

## 2. Типы и ссылки

Control port — `exec`, передаёт только факт выполненной ветки. Data types:
`string`, `number` (finite), `bool`, `config`, `server`, `operation`,
`ssh_target`, `secretref`. Composite config имеет strict provider schema;
server/operation — owner-scoped opaque handle, а не доверенный клиентский
объект с произвольным external ID. Число денег не `number`, а decimal string.

Input binding — ровно один из:

```json
{"literal":"constant"}
```

```json
{"ref":"input.connection_id"}
```

```json
{"ref":"created.server"}
```

`input.*` — объявленные typed run inputs. `<node>.<port>` — output выполненного
узла. `run.current_server` — immutable snapshot текущего managed server,
внедрённый scheduler для entry observe, отсутствует в initial.
`run.current_server`, `run.policy_revision`, `run.episode_watermark` —
единственные runtime refs; клиент не может подменить их. `secretref` вводится
ссылкой на owning credential store, никогда literal password/key.
Не допускаются интерполяция shell из ref, eval/JS и implicit type coercion.

`input.connection_id` имеет тип number, но блок разрешает только ID своей
connection. `input.configuration_ref` тип config: сервер разрешает его в
validated immutable configuration snapshot по owner/revision. Версия text
установки — number, проверяется на format/access. `server` включает только
safe resource identity/IP handles; SSH credentials отдельным secretref.

## 3. Нормативная структурная schema v1

JSON Schema draft 2020-12 ниже определяет структуру. `params` дополнительно
обязательно проверяются strict schema конкретного `type` из block registry
(следующая таблица) с `additionalProperties:false`; одного структурного
валидатора для допуска недостаточно. Это составной нормативный контракт,
а не разрешение произвольных params.

```json
{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "title":"sharedd blueprint v1",
  "type":"object",
  "additionalProperties":false,
  "required":["schema_version","name","provider","inputs","entries","nodes","edges"],
  "properties":{
    "schema_version":{"const":1},
    "name":{"type":"string","minLength":1,"maxLength":100},
    "provider":{"enum":["nodehost","beget"]},
    "inputs":{"type":"object","maxProperties":32,"propertyNames":{"pattern":"^[a-z][a-z0-9_]{0,63}$"},"additionalProperties":{"$ref":"#/$defs/input"}},
    "entries":{"type":"object","additionalProperties":false,"required":["initial","observe"],"properties":{"initial":{"$ref":"#/$defs/id"},"observe":{"$ref":"#/$defs/id"}}},
    "nodes":{"type":"array","minItems":1,"maxItems":128,"items":{"$ref":"#/$defs/node"}},
    "edges":{"type":"array","maxItems":256,"items":{"$ref":"#/$defs/edge"}}
  },
  "$defs":{
    "id":{"type":"string","pattern":"^[a-z][a-z0-9_]{0,63}$"},
    "datatype":{"enum":["string","number","bool","config","server","operation","ssh_target","secretref"]},
    "input":{"type":"object","additionalProperties":false,"required":["type","required"],"properties":{"type":{"$ref":"#/$defs/datatype"},"required":{"type":"boolean"}}},
    "binding":{"oneOf":[{"type":"object","additionalProperties":false,"required":["literal"],"properties":{"literal":{"type":["string","number","boolean"]}}},{"type":"object","additionalProperties":false,"required":["ref"],"properties":{"ref":{"type":"string","pattern":"^[a-z][a-z0-9_]{0,63}\\.[a-z][a-z0-9_]{0,63}$"}}}]},
    "node":{"type":"object","additionalProperties":false,"required":["id","type","params","in"],"properties":{"id":{"$ref":"#/$defs/id"},"type":{"enum":["server.create","server.ready","install.text","pool.admit","event.ip_ban","policy.guard","server.distinct_ip","resource.fence","resource.switch","server.delete","server.absent","observe.arm","ip.order","ip.attach","ip.switch_advertised","ip.cleanup","ip.verify","terminal.stop"]},"params":{"type":"object"},"in":{"type":"object","maxProperties":32,"additionalProperties":{"$ref":"#/$defs/binding"}},"retry":{"type":"object","additionalProperties":false,"required":["max_attempts","backoff_seconds"],"properties":{"max_attempts":{"type":"integer","minimum":1,"maximum":5},"backoff_seconds":{"type":"integer","minimum":1,"maximum":300}}}}},
    "edge":{"type":"object","additionalProperties":false,"required":["from","port","to"],"properties":{"from":{"$ref":"#/$defs/id"},"port":{"type":"string","pattern":"^[a-z][a-z0-9_]{0,31}$"},"to":{"$ref":"#/$defs/id"}}}
  }
}
```

Общий сериализованный graph ≤256KiB UTF-8. Node IDs уникальны; edge control
только к существующим nodes. Data edges представлены `in.ref` и также
учитываются в общем лимите 256 связей (control + bindings на node outputs).
Reserved ID `input`/`run` нельзя использовать для node. Literal secretref,
server/config/operation/ssh_target запрещены: только checked handle refs.

## 4. Allowlisted блоки и параметры

У каждого блока обязательный входной `exec` control, кроме entry node;
он не перечисляется в `in`. Нормальный control output `ok`, если в таблице
не указано другое. Все перечисленные data inputs обязательны. Все params
обязательны, если не сказано иначе; `{}` означает отсутствие любых params.
Retry по умолчанию одна попытка; общий retry object только для read-only
блоков, не для purchases/SSH. Таймеры persisted. Любая неописанная ошибка
останавливает ветку в paused/needs_attention/failed по certainty.

| type | params (строгие) | Data in → out | Effect / ветки |
|---|---|---|---|
| `server.create` | `{purpose:"initial"\|"replacement"}` | connection:number, configuration:config → operation:operation | Одна покупка; output handle persist до dispatch; replacement автоматически связывается с текущим guarded episode/old resource |
| `server.ready` | `{timeout_seconds:600}` | operation → server | Read/poll до ready; timeout → reconciliation, не повтор create |
| `install.text` | `{timeout_seconds:1200}` | server, version:number → server | Запуск pinned text installer, vars из validated run snapshot; секреты server-side; unknown shell не replay |
| `pool.admit` | `{timeout_seconds:600}` | server → server | Fresh independent GP/metrics/TCP admission; не mastery |
| `event.ip_ban` | `{}` | server → server, episode:string | Durable subscription; output только для нового episode после watermark |
| `policy.guard` | `{}` | server, episode:string → server, episode:string | `active` либо `stop`; CAS mode/version/current episode/owner enabled |
| `server.distinct_ip` | `{}` | old:server, new:server → server (new) | Проверить managed IPv4 new != banned IP episode; иначе needs_attention без удаления old |
| `resource.fence` | `{}` | old:server, new:server → server (new) | Проверить new admission evidence; durable fence old |
| `resource.switch` | `{}` | old:server, new:server → server (new) | CAS policy/current, требует fence old |
| `server.delete` | `{}` | server → server | Managed server delete; old admission dependency/fence; Beget DELETE IP action; неизвестный результат reconcile |
| `server.absent` | `{timeout_seconds:600}` | server → server | Подтвердить физическое отсутствие VPS и cleanup obligations |
| `observe.arm` | `{}` | server → нет | Записать subscription successor generation, end run succeeded; не рекурсивный запуск |
| `ip.order` | `{family:"IP_V4"}` | server → server (с новым managed address) | Beget одна покупка; `ok` или `fallback` только при подтверждённом допустимом отказе без денег/unknown |
| `ip.attach` | `{}` | server → server | Уже привязан — read no-op; иначе attach один раз; `ok`/`fallback` после безопасного подтверждённого отказа |
| `ip.switch_advertised` | `{timeout_seconds:1200}` | server → server | Транзакционное изменение OS/proxy/agent advertised IP; same identity; `ok`/`fallback` только после доказанного безопасного rollback |
| `ip.cleanup` | `{}` | server → server | После admission нового IP удалить только old managed IP; primary gate; отдельный cleanup checkpoint |
| `ip.verify` | `{}` | server → server | Только new managed IPv4, old отсутствует в account inventory; чужие IP не трогать |
| `terminal.stop` | `{reason:"policy_inactive"}` | нет → нет | Завершение skipped/cancelled без provider effects |

Существующий SSH-source максимум 64KiB сохраняется в text version; graph
`install.text` не принимает inline source. Core blocks для estimate/catalog,
арифметики/условий, произвольной shell можно добавлять только новой явно
версионированной allowlist с типами, params и security review. В v1 branching
уже реализован typed outcome ports guard и IP blocks; не нужен eval-expression.
`operation`/`ssh_target`/`secretref` присутствуют в type system; это не повод
открывать их произвольную десериализацию из браузера.

### 4.1 Обязательные pre/postconditions

Каждый effect, а не только видимый policy.guard, сверяет policy revision,
активность owner/run и current generation. `server.create(replacement)`
требует dominated guard этого episode, replacement reservation и old resource.
`resource.fence/switch/delete(old)` требуют доказанный admit new и разные IDs.
Initial path не имеет old и не может удалить что-либо.

`ip.order` делает before snapshot. Unknown/402 не выводят `fallback`:
unknown → reconcile; insufficient funds → paused. `ip.attach` сверяет VPS/
region и ownership; приобретённый, но не присоединённый адрес учитывается в
cleanup obligation даже при fallback. Confirmed failure с таким адресом
должен сначала завершить его безопасную очистку или остаться needs_attention;
скрытый платный IP не теряется из учёта.

`ip.switch_advertised`: сохранить OS/proxy/agent конфиги и маршруты,
проверить новый IP на интерфейсе, shared port/listen и SSH reachability;
явно задать `public_ip` агента, не полагаться на echo resolver. Изменение
advertised IP и прокси применить с rollback watchdog и подтверждением по
новому SSH пути до удаления старого. Proxy TOML validation/ownership/mode и
one-shot apply safety transaction сохраняются. Same node re-register с новым
адресом и существующими secret/owner. Если rollback не доказан — unknown,
не fallback create. `pool.admit` после этого проверяет именно новый target
свежим независимым measurement.

`ip.cleanup` запускается только после успешного admission. Отсутствие
promote-primary API — integration gate, не причина придумывать вызов.
Ошибка удаления старого IP retry/reconcile именно cleanup; никогда fallback
на create. `ip.verify` проверяет «только новый **управляемый** IPv4», не
требует удалять пользовательские IPv6/другие IP. Для первичного адреса,
полученного при managed create, provenance записывается в исходной операции.

## 5. Серверная валидация графа

Проверки до publish/run и повторно перед effect:

1. Size/schema/types/unknown keys; allowlist блока и params, provider
   capabilities, лимиты retry/wait. Только initial/observe entry, оба существуют.
2. DAG по control и data dependencies; нет произвольных cycles, parallel
   fan-out (один target на output), недостижимых узлов и dangling ports.
   Разные outcome ports взаимоисключающие, не запускаются параллельно.
3. Для каждого entry отдельно: у каждого required input есть source,
   соответствующий тип, источник доступен на **каждом реально возможном
   пути**. Node-output должен доминировать потребителя либо быть доказуемо
   доступным в этой branch. Ссылка на output неисполненной ветки запрещена.
4. Merge по control допустим без «магического объединения» data outputs.
   Arm получает явный server output своей ветки. В новом successor run
   `run.current_server` создаётся из committed policy; внутри текущего run
   этот runtime snapshot неизменен. Завершённые checkpoints не
   переинтерпретируются задним числом.
5. Effect dominance/postconditions: guard, admit, fence, switch, provenance;
   отсутствие пути cleanup-error → create; отсутствие default fallback
   покупки. `run.current_server` недоступен на initial entry. Первый arm
   связывает policy с явно переданным admitted server.
6. Ownership всех refs, доступность pinned versions, no plaintext secrets,
   no raw HTTP URL. Validator ничего не заказывает и не делает SSH.

UI validation — подсказка; authoritative validation всегда на сервере.

## 6. Полный шаблон Nodehost: новая VPS до удаления старой

Входы одинаковы для initial и successor observe. Initial делает ровно одну
покупку. После initial admission или replacement cleanup `observe.arm`
фиксирует current resource и watermark и планирует новое поколение observe
на **той же immutable версии**, без возврата в initial. Включение PROFILE
на уже существующей ноде начинает сразу observe.

```json
{
  "schema_version":1,"name":"Nodehost: замена при новом ip_ban","provider":"nodehost",
  "inputs":{"connection_id":{"type":"number","required":true},"configuration_ref":{"type":"config","required":true},"installation_profile_version_id":{"type":"number","required":true}},
  "entries":{"initial":"create_initial","observe":"ban"},
  "nodes":[
    {"id":"create_initial","type":"server.create","params":{"purpose":"initial"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration_ref"}}},
    {"id":"ready_initial","type":"server.ready","params":{"timeout_seconds":600},"in":{"operation":{"ref":"create_initial.operation"}}},
    {"id":"install_initial","type":"install.text","params":{"timeout_seconds":1200},"in":{"server":{"ref":"ready_initial.server"},"version":{"ref":"input.installation_profile_version_id"}}},
    {"id":"admit_initial","type":"pool.admit","params":{"timeout_seconds":600},"in":{"server":{"ref":"install_initial.server"}}},
    {"id":"arm_initial","type":"observe.arm","params":{},"in":{"server":{"ref":"admit_initial.server"}}},
    {"id":"ban","type":"event.ip_ban","params":{},"in":{"server":{"ref":"run.current_server"}}},
    {"id":"guard","type":"policy.guard","params":{},"in":{"server":{"ref":"ban.server"},"episode":{"ref":"ban.episode"}}},
    {"id":"stop","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}},
    {"id":"create_new","type":"server.create","params":{"purpose":"replacement"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration_ref"}}},
    {"id":"ready_new","type":"server.ready","params":{"timeout_seconds":600},"in":{"operation":{"ref":"create_new.operation"}}},
    {"id":"distinct","type":"server.distinct_ip","params":{},"in":{"old":{"ref":"guard.server"},"new":{"ref":"ready_new.server"}}},
    {"id":"install_new","type":"install.text","params":{"timeout_seconds":1200},"in":{"server":{"ref":"distinct.server"},"version":{"ref":"input.installation_profile_version_id"}}},
    {"id":"admit_new","type":"pool.admit","params":{"timeout_seconds":600},"in":{"server":{"ref":"install_new.server"}}},
    {"id":"fence","type":"resource.fence","params":{},"in":{"old":{"ref":"guard.server"},"new":{"ref":"admit_new.server"}}},
    {"id":"switch","type":"resource.switch","params":{},"in":{"old":{"ref":"guard.server"},"new":{"ref":"fence.server"}}},
    {"id":"delete_old","type":"server.delete","params":{},"in":{"server":{"ref":"guard.server"}}},
    {"id":"absent","type":"server.absent","params":{"timeout_seconds":600},"in":{"server":{"ref":"delete_old.server"}}},
    {"id":"arm_new","type":"observe.arm","params":{},"in":{"server":{"ref":"switch.server"}}}
  ],
  "edges":[
    {"from":"create_initial","port":"ok","to":"ready_initial"},
    {"from":"ready_initial","port":"ok","to":"install_initial"},
    {"from":"install_initial","port":"ok","to":"admit_initial"},
    {"from":"admit_initial","port":"ok","to":"arm_initial"},
    {"from":"ban","port":"ok","to":"guard"},
    {"from":"guard","port":"stop","to":"stop"},
    {"from":"guard","port":"active","to":"create_new"},
    {"from":"create_new","port":"ok","to":"ready_new"},
    {"from":"ready_new","port":"ok","to":"distinct"},
    {"from":"distinct","port":"ok","to":"install_new"},
    {"from":"install_new","port":"ok","to":"admit_new"},
    {"from":"admit_new","port":"ok","to":"fence"},
    {"from":"fence","port":"ok","to":"switch"},
    {"from":"switch","port":"ok","to":"delete_old"},
    {"from":"delete_old","port":"ok","to":"absent"},
    {"from":"absent","port":"ok","to":"arm_new"}
  ]
}
```

Если new получил banned IP, old остаётся на месте; run needs_attention с
учётом новой VPS. Повторные платные «поиски хорошего IP» автоматически не
делаются. Admission timeout оставляет new и old учтёнными; cleanup old до
admit запрещён. Перед каждым шагом проверяются новые mode/revision, а не
только старый guard output. Без нового бан-эпизода повторной покупки нет.

## 7. Полный шаблон Beget: сначала IP, затем допустимый fallback VPS

Initial тот же: create → ready → install → admit → arm. Observe пробует
добавить IPv4 с сохранением old, переключить advertised IP, independently
admit и удалить old. Confirmed безопасный отказ order/attach/switch может
пойти в replacement. Unknown, недостаток средств, неясный rollback и cleanup
ошибки не имеют fallback edge.

```json
{
  "schema_version":1,"name":"Beget: IPv4 либо связанная замена VPS","provider":"beget",
  "inputs":{"connection_id":{"type":"number","required":true},"configuration_ref":{"type":"config","required":true},"installation_profile_version_id":{"type":"number","required":true}},
  "entries":{"initial":"create_initial","observe":"ban"},
  "nodes":[
    {"id":"create_initial","type":"server.create","params":{"purpose":"initial"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration_ref"}}},
    {"id":"ready_initial","type":"server.ready","params":{"timeout_seconds":600},"in":{"operation":{"ref":"create_initial.operation"}}},
    {"id":"install_initial","type":"install.text","params":{"timeout_seconds":1200},"in":{"server":{"ref":"ready_initial.server"},"version":{"ref":"input.installation_profile_version_id"}}},
    {"id":"admit_initial","type":"pool.admit","params":{"timeout_seconds":600},"in":{"server":{"ref":"install_initial.server"}}},
    {"id":"arm_initial","type":"observe.arm","params":{},"in":{"server":{"ref":"admit_initial.server"}}},
    {"id":"ban","type":"event.ip_ban","params":{},"in":{"server":{"ref":"run.current_server"}}},
    {"id":"guard","type":"policy.guard","params":{},"in":{"server":{"ref":"ban.server"},"episode":{"ref":"ban.episode"}}},
    {"id":"stop","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}},
    {"id":"order_ip","type":"ip.order","params":{"family":"IP_V4"},"in":{"server":{"ref":"guard.server"}}},
    {"id":"attach_ip","type":"ip.attach","params":{},"in":{"server":{"ref":"order_ip.server"}}},
    {"id":"switch_ip","type":"ip.switch_advertised","params":{"timeout_seconds":1200},"in":{"server":{"ref":"attach_ip.server"}}},
    {"id":"admit_ip","type":"pool.admit","params":{"timeout_seconds":600},"in":{"server":{"ref":"switch_ip.server"}}},
    {"id":"cleanup_ip","type":"ip.cleanup","params":{},"in":{"server":{"ref":"admit_ip.server"}}},
    {"id":"verify_ip","type":"ip.verify","params":{},"in":{"server":{"ref":"cleanup_ip.server"}}},
    {"id":"arm_ip","type":"observe.arm","params":{},"in":{"server":{"ref":"verify_ip.server"}}},
    {"id":"create_new","type":"server.create","params":{"purpose":"replacement"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration_ref"}}},
    {"id":"ready_new","type":"server.ready","params":{"timeout_seconds":600},"in":{"operation":{"ref":"create_new.operation"}}},
    {"id":"distinct","type":"server.distinct_ip","params":{},"in":{"old":{"ref":"guard.server"},"new":{"ref":"ready_new.server"}}},
    {"id":"install_new","type":"install.text","params":{"timeout_seconds":1200},"in":{"server":{"ref":"distinct.server"},"version":{"ref":"input.installation_profile_version_id"}}},
    {"id":"admit_new","type":"pool.admit","params":{"timeout_seconds":600},"in":{"server":{"ref":"install_new.server"}}},
    {"id":"fence","type":"resource.fence","params":{},"in":{"old":{"ref":"guard.server"},"new":{"ref":"admit_new.server"}}},
    {"id":"switch","type":"resource.switch","params":{},"in":{"old":{"ref":"guard.server"},"new":{"ref":"fence.server"}}},
    {"id":"delete_old","type":"server.delete","params":{},"in":{"server":{"ref":"guard.server"}}},
    {"id":"absent","type":"server.absent","params":{"timeout_seconds":600},"in":{"server":{"ref":"delete_old.server"}}},
    {"id":"arm_new","type":"observe.arm","params":{},"in":{"server":{"ref":"switch.server"}}}
  ],
  "edges":[
    {"from":"create_initial","port":"ok","to":"ready_initial"},
    {"from":"ready_initial","port":"ok","to":"install_initial"},
    {"from":"install_initial","port":"ok","to":"admit_initial"},
    {"from":"admit_initial","port":"ok","to":"arm_initial"},
    {"from":"ban","port":"ok","to":"guard"},
    {"from":"guard","port":"stop","to":"stop"},
    {"from":"guard","port":"active","to":"order_ip"},
    {"from":"order_ip","port":"ok","to":"attach_ip"},
    {"from":"order_ip","port":"fallback","to":"create_new"},
    {"from":"attach_ip","port":"ok","to":"switch_ip"},
    {"from":"attach_ip","port":"fallback","to":"create_new"},
    {"from":"switch_ip","port":"ok","to":"admit_ip"},
    {"from":"switch_ip","port":"fallback","to":"create_new"},
    {"from":"admit_ip","port":"ok","to":"cleanup_ip"},
    {"from":"cleanup_ip","port":"ok","to":"verify_ip"},
    {"from":"verify_ip","port":"ok","to":"arm_ip"},
    {"from":"create_new","port":"ok","to":"ready_new"},
    {"from":"ready_new","port":"ok","to":"distinct"},
    {"from":"distinct","port":"ok","to":"install_new"},
    {"from":"install_new","port":"ok","to":"admit_new"},
    {"from":"admit_new","port":"ok","to":"fence"},
    {"from":"fence","port":"ok","to":"switch"},
    {"from":"switch","port":"ok","to":"delete_old"},
    {"from":"delete_old","port":"ok","to":"absent"},
    {"from":"absent","port":"ok","to":"arm_new"}
  ]
}
```

`create_new` не читает outputs IP ветки: только общие inputs и guarded server
через проверенный execution context. Так исключена зависимость от inactive
branch. При успешной смене IP arm остаётся на той же VPS/identity; при замене
arm переходит на новую. Ни одна ошибка cleanup не создаёт ещё одну VPS.
До закрытия Beget primary-IP gate шаблон остаётся draft; можно отдельно
выбрать профиль замены VPS без IP-ветки, но нельзя молча заменить выбранный
пользователем blueprint другой логикой.
