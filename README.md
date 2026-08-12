# sharedd — ротация прокси через регистратор и Cloudflare DNS

Высокодоступный пул MTProto-прокси ([telemt](https://github.com/telemt/telemt)) с автоматической
ротацией клиентов между нодами: регистратор следит за здоровьем нод и держит A-запись **каждого**
managed-домена указанной на свою активную ноду (per-domain мастера). При падении/блокировке
ноды-мастера DNS её доменов переключается на следующих по очереди здоровья; нода, потерявшая
мастерство по любой причине, возвращается в конец очереди.

Поддержка популярных фиксов:
1. [MTProxyL](https://github.com/Liafanx/MTProxyL/)
2. [Fix by MEKO](https://github.com/Liafanx/MTProxyL/](https://github.com/Mekotofeuka/MTPROTO_FIX_By_MEKO/))

## Архитектура

```
                 ┌──────────────────────────┐
                 │        Registry          │   (единая точка принятия решений)
                 │  - пул кандидатов        │        ▲ register/heartbeat/report
                 │  - TCP-probe             │        │ /config (секреты, SNI, интервалы)
                 │  - верификация Globalping│   ┌────┴─────┐  ┌──────────┐  ┌──────────┐
                 │  - выбор активной ноды   │   │ node-a   │  │ node-b   │  │ node-c   │ ...
                 │  - Cloudflare DNS update │   │ telemt + │  │ telemt + │  │ telemt + │
                 └────────────┬─────────────┘   │ agent    │  │ agent    │  │ agent    │
                              │ A-record        └──────────┘  └──────────┘  └──────────┘
                              ▼
                      mtp1.example.com  →  текущая активная нода
```
