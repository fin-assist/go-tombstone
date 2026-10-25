# go-tombstone

Tombstone удалённых пользователей: после `PurgeUserData` сервис помнит `user_id` и отклоняет вызовы от его имени (`TOKEN_REVOKED`), а опоздавшие события и конкурентные записи не воскрешают данные. Нужна `user`, `bank-statement-data`, `analytics` и `assistant`. Задача — [FIN-110](https://finassist.youtrack.cloud/issue/FIN-110).

Основана на [`fin-assist/go-library-template`](https://github.com/fin-assist/go-library-template) ([FIN-118](https://finassist.youtrack.cloud/issue/FIN-118)).

- [docs/releasing.md](docs/releasing.md) — версии, что несовместимо, CHANGELOG, выпуск, миграции, Renovate.
- [CHANGELOG.md](CHANGELOG.md) — изменения по версиям.
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Раскладка

```
doc.go               package documentation: what the library does and what it needs from the service
migrations.go        Migrations, MigrationsName, MigrationsTable for the service's migration runner
migrations/          SQL migrations of the library, embedded (NNN_name.up.sql / .down.sql)
internal/pgtest/     PostgreSQL for tests and the up -> down -> up check, same rules as the service runner
scripts/             native statements coverage and suite guards, CHANGELOG checks (Python, standard library)
docs/releasing.md    versions and releases
```

Код библиотеки — в корневом пакете (импорт `github.com/fin-assist/go-tombstone` даёт `tombstone.New`); подпакеты — когда у частей разные потребители (`identity/grpcserver` и `identity/httpmw`), внутренние детали — в `internal/`.

## Внедрение

Библиотека используется в `user`, `bank-statement-data`, `analytics`, `assistant`. Она не
открывает транзакций, не читает JWT, не извлекает идентичность из metadata, не запускает
фоновых процессов и не вызывает соседние сервисы. После выпуска владелец ставит тег;
сервис устанавливает его через `go get github.com/fin-assist/go-tombstone@vX.Y.Z`.
Зависимости: опубликованный `contracts/gen/go v0.4.0` (protobuf и `finerr`), pgx,
Prometheus, `google/uuid` (разбор и нормализация UUID, которых нет в stdlib).
`go-identity v0.1.0` используется в examples и usage-тесте; production API зависит от
маленького адаптера, который читает `id, _ := identity.UserID(ctx)` и возвращает id.

### Миграции и сборка

В `MigrationSources()` сервиса источник библиотеки ставится перед собственным:

```go
{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
{Name: "service", Table: "schema_migrations", FS: migrations.FS},
```

`001_users` создаёт `tombstone_users(user_id uuid PRIMARY KEY, deleted_at timestamptz NOT NULL)`.
У версии миграций своя таблица `tombstone_schema_migrations`; есть down-файл.
В `wire()` передайте `postgres.DB` как `Transactions`, соединение **primary** как `Reader`
и сервисные часы. Конструктор `New(db, primary, clock)` возвращает `(*Store, error)`.
Сервисный адаптер `Within(ctx, fn)` для `Transactor` должен вызвать `postgres.DB.Run`
с READ COMMITTED, откатить транзакцию при любой ошибке и вернуть nil только после commit.
`Tx(ctx)` и callback обязаны видеть одну и ту же транзакцию в контексте.
Полный рабочий адаптер и сборка gRPC показаны в `integration_test.go`, `TestIntegrationServiceUsageOverGRPC`.

### Композиция с go-postgres

Сервис может передать общий [`go-postgres.Manager`](https://github.com/fin-assist/go-postgres)
как `Transactions`. Опубликованная `v0.1.2` также освобождает транзакцию при
`runtime.Goexit`. Зависимость используется здесь только в тестах и примере;
публичные интерфейсы tombstone не требуют конкретного менеджера.
Для `DeletionServer` достаточно адаптера имени метода:

```go
type deletionTransactions struct { *postgres.Manager }

func (t deletionTransactions) Within(ctx context.Context, fn func(context.Context) error) error {
    return t.RunWith(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

manager := postgres.New(primary) // github.com/fin-assist/go-postgres
store, err := tombstone.New(manager, primary, clock) // check err
runner := deletionTransactions{manager}
// Pass runner to NewDeletionServer; removal uses manager.Tx(ctx).
```

Этот же экземпляр Manager передают адаптерам бизнес-записей. Запись вызывает
`store.Guard(ctx, userID)` внутри `Run` **до** изменения данных и использует
`manager.Tx(ctx)`. Отдельный менеджер для удаления не создаётся: другой экземпляр
не принимает чужой транзакционный контекст. Вложенный `Run` присоединяется к внешней
транзакции без промежуточного commit; окончательное решение принимает её владелец.
Внешний вызов должен выбирать READ COMMITTED: параметры вложенного `RunWith`
не меняют уже начатую транзакцию, а Guard отклоняет неподходящую изоляцию.

`fin244_manager*_test.go` проверяют эту сборку через публичный API и настоящий
PostgreSQL: атомарный commit/rollback бизнес-записей и tombstone, отложенный FK на
фактическом COMMIT, конкуренцию удаления с записью, чужой Manager и освобождение
соединения после panic, отмены и Goexit. Тест gRPC проверяет обезличенный INTERNAL
при ошибке COMMIT и повтор после успешного удаления; identity/ACL остаются
обязанностью сервиса. Пример — `ExampleNew_goPostgres`.

### Интерцепторы и удаление

```go
counter, err := tombstone.NewRevokedCounter(registry) // check err
verifiedUser := func(ctx context.Context) string { id, _ := identity.UserID(ctx); return id }
unary, stream, err := tombstone.Interceptors(store, verifiedUser, counter) // check err
// verifiedUser reads the subject from verified identity context; empty means system call.
// identity/service ACL -> user identity -> tombstone -> protovalidate -> handler
server := grpc.NewServer(
    grpc.ChainUnaryInterceptor(identityUnary, unary, validationUnary),
    grpc.ChainStreamInterceptor(identityStream, stream, validationStream),
)
deletions, err := tombstone.NewDeletionServer(transactionRunner, store,
    func(ctx context.Context, userID string) error {
        tx, ok := db.Tx(ctx)
        if !ok { return tombstone.ErrTransactionRequired }
        _, err := tx.Exec(ctx, "DELETE FROM service_records WHERE user_id=$1", userID)
        return err
    }) // check err
userdeletionv1.RegisterUserDeletionServiceServer(server, deletions)
```

Для `PurgeUserData` service ACL разрешает только проверенный сервис `user`.
Библиотека не заменяет этот ACL: внешний запрос без идентичности нельзя трактовать
как системный. `verifiedUser` использует проверенный `sub`; клиентские metadata и
`user_id` запроса для пользовательской модели не являются источником идентичности.
Удаляются **все** сервисные данные пользователя, включая отложенные локальные задания.
Данные и tombstone записываются одной транзакцией. При ошибке callback или записи
сервис откатывает всё; наружу RPC отдаёт обезличенный INTERNAL. Отмена и дедлайн
самого запроса сохраняют CANCELED/DEADLINE_EXCEEDED с фиксированным текстом контекста;
внутренний таймаут сервиса при живом запросе остаётся обезличенным INTERNAL. Повтор после commit
возвращает OK, callback повторно не выполняется. `user.DeleteAccount` вызывает `Purge`
в своей транзакции вместе с отзывом refresh-токенов и созданием outbox.

Абзац об `Interceptors` ниже — только для сервера без go-grpc (с ним — «Сборка с go-grpc»); `NewDeletionServer`, регистрация `UserDeletionService` и ACL `PurgeUserData` выше нужны в обоих случаях. Unary и stream интерцепторы читают primary при каждом новом вызове. Tombstone даёт
`UNAUTHENTICATED` с `ErrorInfo{domain: "finassist", reason: "TOKEN_REVOKED"}` через `finerr`.
Сбой чтения закрывает доступ (INTERNAL), обработчик не вызывается. Отмена и дедлайн
самого запроса сохраняют собственные коды и не считаются ошибками хранилища. Текст
обёрнутых ошибок checker/transaction runner/callback никогда не передаётся в RPC-статус. **Кэша нет**:
отрицательная запись в поде скрыла бы удаление, сделанное другим подом. У уже открытого
потока admission не повторяется: каждая его запись и фоновая работа обязаны пройти Guard.
Поток, открытый до удаления, может завершить ранее начатое чтение; новый поток уже отклонён.
До доставки `PurgeUserData` остаётся контрактное окно; мгновенный отзыв — после MVP.

### Сборка с go-grpc

Сервис, который собирает gRPC-сервер через [`go-grpc/server`](https://github.com/fin-assist/go-grpc)
(FIN-250), **не** ставит `tombstone.Interceptors` в цепочку: у go-grpc своя проверка
tombstone после identity и ACL, она вызывает `server.Tombstones.IsDeleted`
для моделей `ModelUser` и `ModelMixed` с токеном пользователя. Достаточно адаптера над `Store.Deleted`:

```go
// deletedUsers adapts the primary-backed tombstone check to go-grpc admission.
type deletedUsers struct{ store *tombstone.Store }

func (d deletedUsers) IsDeleted(ctx context.Context, id string) (bool, error) {
    return d.store.Deleted(ctx, id) // primary, no cache
}

srv, err := server.New(opts, server.Deps{ /* ... */ Tombstones: deletedUsers{store}})
```

- Адаптер не кэширует ответ и читает через тот же `Reader` на primary, что передан в `New`:
  отрицательный кэш или реплика скрыли бы удаление, сделанное другим подом.
- Двойная проверка не нужна: вместе с go-grpc `Interceptors` библиотеки не ставятся.
- Отличия от `Interceptors`: сбой чтения tombstone go-grpc отдаёт голым `UNAVAILABLE`
  (обработчик не вызван, повтор безопасен), а не `INTERNAL`; `TOKEN_REVOKED` строит сам go-grpc
  через `finerr`. Любая ошибка `IsDeleted`, включая отмену запроса, у go-grpc — тот же `UNAVAILABLE`. В том числе `ErrUserID`: проверенный `sub`, который не является UUID, даёт `UNAVAILABLE`, а не `INTERNAL`.
- `Rule.AllowDeleted` go-grpc пропускает удалённого пользователя только на явных
  cleanup-методах (например, `user` `DeleteAccount`), но `IsDeleted` вызывается и для них.
  Поэтому адаптер не должен считать `finassist_tombstone_revoked_total` в сервисе с такими
  методами: в счётчик попали бы разрешённые вызовы. Отказы admission видны в RED-метрике
  go-grpc `grpc_server_handled_total{reason="TOKEN_REVOKED"}`; счётчик библиотеки остаётся для `Interceptors` и отказов
  после Guard. Без `AllowDeleted` адаптер может увеличивать его при `deleted == true`.
- `DeletionServer` регистрируется на go-grpc сервере так же (`userdeletionv1.RegisterUserDeletionServiceServer(srv.GRPC(), deletions)`),
  а в `AccessPolicy()` для `PurgeUserData` нужно правило `{Callers: []string{"user"}, Model: server.ModelSystem}` — только вызывающий `user`.
- Системные вызовы (`ModelSystem`) go-grpc на admission не проверяет — как и `Interceptors`
  с пустым `verifiedUser`; их записи защищает Guard (ниже).

### Все записи пользователя и получатели outbox

В каждой пишущей транзакции **до записи данных** вызовите:

```go
alive, err := store.Guard(ctx, userID)
if err != nil { return err }
if !alive {
    return nil // system receiver: acknowledge a late event without any effects
}
// Write the effect and any outbox rows using db.Tx(ctx), then commit in the service.
```

Пользовательский обработчик при `!alive` вместо nil возвращает `finerr.TokenRevoked()`
и увеличивает тот же `counter`: это отказ после гонки с admission. У системного
получателя OK не означает применение события: tombstone намеренно подавляет эффект.
При живом пользователе проверка версии и основное действие выполняются после Guard.
Сетевых вызовов внутри транзакции нет.

Guard и Purge берут `pg_advisory_xact_lock(hashtextextended(user_id, 110))` и держат до
commit/rollback сервиса. Корректный UUID нормализуется через `uuid.Parse(...).String()`
перед lock, чтением и записью tombstone; верхний регистр допускается.
Callback `Purge(ctx, id, func(ctx, canonicalUserID) error)` получает ту же нормализованную
форму: удаление использует переданный canonicalUserID. Пользовательские ключи новых
таблиц должны иметь тип **uuid**. Для legacy text-ключей сервис обязан нормализовать ID
также на всех путях записи: один Guard не меняет последующий SQL вызывающего.
Разные формы UUID всегда получают одну блокировку. Коллизия hash только
сериализует двух независимых пользователей. Ключ seed 110 зарезервирован для этого
протокола; сервис не должен использовать его для другого порядка блокировок.

После ожидания блокировки отдельный SELECT проверяет tombstone. Обязательно
**READ COMMITTED**: при REPEATABLE READ/Serializable старая транзакция могла бы не
увидеть новое удаление; Guard проверяет `SHOW transaction_isolation` и возвращает
`ErrIsolation`. Сервисные блокировки строк берите после Guard; при работе с несколькими
пользователями — в стабильном порядке UUID. Нельзя оборачивать чужой код Guard после
уже сделанных записей или запускать callback в другой транзакции.

Если запись началась раньше Purge, удаление ждёт её commit и затем удаляет её данные.
Если Purge первым взял lock, запись ждёт и затем видит tombstone; ничего не создаётся.
Это прикладной протокол: прямой SQL и писатели без Guard его обходят. FK, триггеров,
CHECK/EXCLUDE и хранимых функций нет — по архитектурному решению проекта.

### Хранение и наблюдаемость

Tombstone хранится **бессрочно**. Access-токен живёт 15 минут, но максимальная задержка
outbox не ограничена: конечный TTL снова разрешил бы позднее событие после простоя
или восстановления. 30 дней хранения бэкапов не ограничивают задержку outbox и не
задают TTL. Нельзя переиспользовать удалённый user_id для нового аккаунта.
После восстановления БД необходимо воспроизвести актуальные удаления до открытия
записей и доставки событий; восстановление старого снимка без tombstone библиотека
самостоятельно обнаружить не может. Down удаляет защиту: только для отката пустой БД,
не как production-очистка.

Метрика `finassist_tombstone_revoked_total` без меток считает отказы пользовательских
вызовов при admission через `Interceptors` (с go-grpc — см. «Сборка с go-grpc»); для отказов Guard сервис увеличивает тот же counter. Реестр
передаётся в `NewRevokedCounter`; повторная регистрация возвращает ошибку. Настройте
алерт на ошибки tombstone-хранилища и сбои/задержку доставки Purge; user_id, SQL,
токены и DSN не попадают в ошибки библиотеки или метки. Библиотека не ведёт логов.

### Чек-лист новой таблицы или обработчика

- Таблица с user_id включена в callback PurgeUserData, её фоновые задания отменяются там же.
- Любой путь записи (RPC, поток, фон, миграционный backfill, событие) вызывает Guard
  в той же READ COMMITTED транзакции, что и эффект, и корректно обрабатывает `!alive`.
- Чтения новых RPC проходят tombstone admission после identity/ACL: либо `tombstone.Interceptors`, либо шаг go-grpc через `server.Tombstones` — не оба сразу.
- Системный receiver после tombstone возвращает OK без эффекта; пользовательский путь — TOKEN_REVOKED.
- Новая зависимая таблица покрыта тестом удаления, повтора, rollback и конкурентной записи.

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | `go mod tidy` без изменений, `go vet`, `go test -race` с PostgreSQL 18 в Docker (testcontainers), включая «вверх → вниз → вверх» для миграций; тесты скриптов; формат `CHANGELOG.md`; три независимых statements gate80, профили и отчёты в artifacts/summary |
| `lint` | golangci-lint (`.golangci.yml`): `forbidigo` против обхода `finerr` и против глобальных логгера, реестра метрик и переменных окружения; `depguard` против фреймворков |
| `api` | `gorelease`: изменения экспортируемого API против последнего тега и предлагаемая версия; на PR выпуска — проверка номера из `CHANGELOG.md` |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Тег `vX.Y.Z` запускает `.github/workflows/release.yml`: проверка тега и GitHub Release с текстом из `CHANGELOG.md`.

Локально:

```bash
make test          # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
make cover         # independent unit/integration/API total80 gates
make api           # API changes against the latest tag
make release-check VERSION=v0.2.0
```

Каждый уровень независимо требует минимум 80 % statements всего production-кода; required check `go` проверяет пороги автоматически. Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в `Makefile` и CI).

### Независимое покрытие statements (FIN-243)

Команды и точная область инструментирования — [docs/testing.md](docs/testing.md).

`make cover-unit`, `make cover-integration` и `make cover-api` независимо требуют **80 % statements** production-кода всех пакетов `./...`, кроме единственного согласованного тестового helper `internal/pgtest`. Unit работает без сети и БД; integration проверяет реальные зависимости; API использует публичный Go API из внешнего тестового пакета и caller-owned PostgreSQL-транзакции. Существующие внутренние usage-тесты остаются integration.

Коллекторы используют одинаковый список `-coverpkg`: штатный `go list -race ./...` перечисляет пакеты, исключается только точное имя `<module>/internal/pgtest`; одноимённые соседние пакеты остаются. Suite по-прежнему запускает `./...`, включая тесты helper. Analyzer не скрывает файлы. Коллекторы используют `-race -count=1 -timeout=8m` и селекторы `TestUnit`/`FuzzUnit`, `TestIntegration`/`FuzzIntegration`, `TestAPI`/`FuzzAPI`; исполняемые `Example` входят в API (обычные примеры без Output только компилируются). `make test-names` проверяет race inventory перед каждым коллектором. JSON execution guard требует реально выполненные успешные Test и отклоняет fail/skip; profile guard отклоняет отсутствующий, пустой, header-only и zero-statement профиль. Обычные пакеты без тестов допустимы. `CI=true` делает недоступную БД ошибкой.

Профили собираются Go из `go.mod`. Только анализатор запускается с `GOTOOLCHAIN=auto`, поскольку требует Go1.27+, даже когда `setup-go` задаёт local; закреплён `go-test-coverage/v2 v2.20.1`; принято его округление **79,95 % → 80,0 %**. `make cover` собирает три профиля и проверяет каждый, `make cover-combined` строит только информационное объединение встроенным comma-profile механизмом инструмента. Низкий уровень нельзя компенсировать combined. Профили `unit.out`, `integration.out`, `api.out`, JSON и штатные отчёты сохраняются как artifacts и в summary. Required check `go` падает при ошибке любого suite/gate; PR-write права и комментарии coverage не нужны.

`make test` сохраняет race-тесты всех уровней, vet, Python-тесты скриптов и CHANGELOG; `make lint`, `make api` и release/migration/Renovate checks сохраняются. Все тестовые запуски ограничиваются временем процесса. БД локально: Docker PostgreSQL18 или `TEST_DATABASE_URL`; пропуск не является успешной проверкой.
