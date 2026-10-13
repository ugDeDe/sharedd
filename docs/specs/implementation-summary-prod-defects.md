# implementation-summary-prod-defects.md — три продакшн-дефекта (ветка `dev`)

Не коммичено, рабочее дерево `dev`. Ничего не откатывалось.

## 1. Панель не персистит конфиг (`ProtectSystem=strict`, `/etc` read-only)

- `systemd/sharedd-registry.service`: добавлена вторая строка
  `ReadWritePaths=-/etc/sharedd` (при сохранённом `ReadWritePaths=-/var/lib/sharedd` и
  `ProtectSystem=strict`). Комментарий обновлён.
- `scripts/install_registry.sh`: тот же `ReadWritePaths=-/etc/sharedd` в unit;
  запись unit вынесена в `write_registry_unit()` — идемпотентно (`cmp -s` → no-op),
  существующий unit бэкапится перед заменой (`…service.bak-<ts>`), вызывается
  `systemctl daemon-reload` только при изменении. Это покрывает свежую установку и
  апгрейд (включая ручной/старый unit без записи в `/etc/sharedd`).
- `registry/internal/server/config_editor.go` — `handlePutConfig`: при ошибке
  персиста теперь `500 {ok:false, persisted:false, error:"не сохранено: …",
  message:"Изменения применены, но не записаны в <path>: …", changed:[…]}` вместо
  `200 ok:true persisted:false`. Рантайм по-прежнему применяется.
- Тест `TestConfigPersistFailureStillApplies` обновлён: ждёт 500 + `ok:false` +
  «не сохранено», рантайм всё равно применён.
- `docs/api-contract.md` §8.1 — описан новый конверт ответа.

## 2. SRMD писал цепочки CNAME (свёрнутое указывало на свёрнутое)

- `registry/internal/server/srmd.go`:
  - `srmdResolveSurvivor` — чистый обход цепочки до конечного несвёрнутого домена;
    цикл/self → `ok=false`.
  - `srmdPickFoldTarget` — наименее загруженный выживший, никогда не сам домен и
    никогда не другой сворачиваемый.
  - `srmdFoldLocked` использует `srmdPickFoldTarget`; цель всегда из survivors
    (несвёрнутых) → цепочка CNAME→CNAME невозможна.
  - `srmdNormalizeCNameChainsLocked` — каждый тик чинит унаследованные цепочки
    (shared3→shared2→shared1 → shared3→shared1), ловит цикл (снимает свёртку).
    Вызывается до и после решения о fold/expand в `srmdRebalanceLocked`, поэтому
    уже испорченный prod-state выправляется без ручного вмешательства.
- Тесты: `TestSRMDResolveSurvivorNoChainNoCycle` (таблица порядков/циклов),
  `TestSRMDFoldNeverCreatesCNAMEChain` (два fold подряд воспроизводят прод-цепочку,
  после ремонта цепочки нет), `TestSRMDRepairsLegacyCNAMEChain` (унаследованная
  цепочка чинится на тике), `assertNoCNAMEChain` (инвариант).

## 3. Флап fold/unfold на границе `required` — откат неверного кулдауна

- Единственная защита от флапа — окно устойчивости `srmdStableTicks = 20`
  подряд тиков с неизменным условием (~минута при `selection_interval_ms =
  3000`). Счётчики in-memory, рестарт их обнуляет (осознанно, как `ttlOverdue`).
- Ранее в этой ветке был добавлен `srmdActionCooldown = 20 * time.Minute` и
  персистентное `SRMDState.LastActionAt`. Владелец подтвердил, что такого
  кулдауна в системе никогда не было — изменение откачено: константа и поле
  state удалены (формат на диске обратно совместим — незнакомое legacy-поле
  `last_action_at` при загрузке игнорируется).
- Ёмкость (`capacity = max_nodes_per_domain × masters_per_domain`) и логика
  мульти-мастер-слотов НЕ тронуты.
- Тесты: `TestSRMDStableWindowSatisfiedTriggersAction` — окно набрано → действие
  есть; `TestSRMDOscillationShorterThanWindowDoesNotAct` — колебание короче окна
  не даёт ни одного действия (счётчик подряд обнуляется при смене условия).

## Verification

```
cd registry && gofmt -l . && go vet ./... && go test -race -count=1 ./... \
  && go build -mod=readonly ./... && ~/go/bin/golangci-lint run ./...
bash -n scripts/*.sh && bash scripts/check_installer_parity.sh
```

Итог: gofmt clean, vet clean, `go test -race` ok (server/state/control/provision/…),
build ok, golangci-lint 0 issues, bash -n ok, parity ok. shellcheck в среде
отсутствует (не запускался).
