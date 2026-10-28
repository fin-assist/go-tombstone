# Изменения

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии и выпуск — [docs/releasing.md](docs/releasing.md). Каждый PR библиотеки, включая документацию, тесты и CI, добавляет строку в «Unreleased»; исключение — полностью ботские PR Renovate только с зависимостями, их сводку по `go.mod` добавляет PR выпуска. При выпуске раздел получает номер версии и дату.

## [Unreleased]

### Изменено

- FIN-372: CI и конфигурации линтеров получают общий источник `fin-assist/go-ci` по неизменяемому SHA; локальные команды используют тот же профиль, обновление и откат выполняются одним PR. Runtime и публичный API не меняются.


## [v0.1.1] — 2026-10-26

### Изменено

- FIN-341: правила lint о процессном состоянии приведены к ядру go-library-template (FIN-322): секции `forbidigo` и `depguard` в `.golangci.yml` и `scripts/test_lint_policy.py` совпадают с шаблоном. Добавлены запреты `slog.With` и `os.Exit`; сняты правила вне ядра: `grpclog` и `grpclog/glogger`, `prometheus.Unregister`, `http.DefaultTransport`/`DefaultServeMux`/`PostForm`, `otel.Tracer`/`Meter`/`Handle` и `otel/log/global`, `os.UserHomeDir`/`UserConfigDir`/`UserCacheDir`, `os.TempDir`/`CreateTemp`/`MkdirTemp`, `(*os.File).Chdir`, `runtime.SetDefaultGOMAXPROCS`, `syscall`/`golang.org/x/sys`; принцип в целом проверяется на ревью. Runtime и экспортируемый API не менялись.
- FIN-336 (FIN-308, FIN-309): из go-library-template перенесены запреты `forbidigo`/`depguard` на глобальное состояние (FIN-294) с `scripts/test_lint_policy.py`, защита CI (FIN-301: `persist-credentials: false` у checkout в `lint`, `api`, `migrations`; `github.base_ref` передаётся через `env`) с `scripts/test_workflow_hardening.py`, правила `docs/releasing.md` и вступление CHANGELOG (FIN-307: строка в `[Unreleased]` от каждого PR, заголовок PR выпуска с ключом FIN, сводка Renovate-изменений `go.mod` в PR выпуска). Нарушений новых запретов в коде не найдено; runtime и экспортируемый API не менялись.
- FIN-319: `internal/pgtest` больше не копирует go-migrate (по FIN-310): оставлен только выбор тестового PostgreSQL (`TEST_DATABASE_URL`/testcontainers, skip локально, ошибка в CI), создание БД, правила файлов и обход up → down → up, а также применение источника в usage-тестах (`Apply`) делегированы `github.com/fin-assist/go-migrate` v0.1.1. go-migrate импортируется только тестами, но как require модуля входит в граф модулей и повышает минимальную версию go-migrate у сервисов, импортирующих библиотеку (сервисы уже на v0.1.1). Отрицательный интеграционный тест закрепляет, что `Apply` завершает тест при ошибке `migrate.Up`. Исключение `internal/pgtest` из покрытия не изменилось; runtime и экспортируемый API не менялись.
- FIN-243: независимые unit/integration/API statements gates 80 % production-пакетов (по согласованию исключён только internal/pgtest), защита от пустых suite и содержательные тесты публичного API; прежний coverage-diff удалён. Runtime и экспортируемый API не изменены.
- FIN-312: записи, вошедшие в уже выпущенный тег v0.1.0, перенесены из «Unreleased» в секцию v0.1.0; в docs/releasing.md записано исключение для GitHub Release v0.1.0. Runtime, API и теги не изменены.
- FIN-295: README «Внедрение» описывает подключение через go-grpc `server.Tombstones`: адаптер над `Store.Deleted` без кэша, отличие кода сбоя (`UNAVAILABLE`), учёт метрики при `AllowDeleted`. Runtime и экспортируемый API не изменены.
- Обновления Renovate после v0.1.0: `contracts/gen/go` v0.4.0 → v0.5.0, `go-identity` v0.1.0 → v0.2.0; добавленные тестовые зависимости из FIN-244/FIN-319 обновлены до `go-postgres` v0.3.0 и `go-migrate` v0.2.0. `prometheus/client_golang` v1.23.2 → v1.25.0, транзитивные `prometheus/client_model` v0.6.2 → v0.6.3, `prometheus/common` v0.66.1 → v0.72.0, `prometheus/procfs` v0.16.1 → v0.22.0; неиспользуемый indirect `go.yaml.in/yaml/v2` удалён из go.mod. Минимальные версии в графе модулей повышены; директива Go и мажорные пути зависимостей не менялись.

### Добавлено

- FIN-244: пример и public-consumer проверки композиции с опубликованным go-postgres v0.1.2: единая сервисная транзакция, откат при ошибке callback/COMMIT, вложенные и чужие контексты, конкуренция Guard/Purge и освобождение соединения после panic/отмены/Goexit; production API и схема не изменены.

### Исправлено

- FIN-264: проверка версии в CI сохраняет сравнение с предыдущим выпуском при публикации тега во время gorelease; чужие и изменившиеся теги отклоняются. Runtime и публичный API не меняются.

## [v0.1.0] — 2026-10-23

### Изменено

- FIN-214: правило Squawk prefer-bigint-over-int отключено для репозитория; размер целочисленного типа выбирается по диапазону домена.

### Добавлено

- FIN-110: tombstone удалённых пользователей, атомарный PurgeUserData, unary/stream TOKEN_REVOKED, транзакционная защита Guard от конкурентного воскрешения и метрика отказов; миграции и инструкция внедрения. UUID нормализуется для блокировки, tombstone и callback удаления; отмены и дедлайны сохраняют gRPC-коды.

### Исправлено

- FIN-288: дедлайн интеграционных и API-фикстур начинается после подготовки PostgreSQL и миграций — холодный старт testcontainers больше не роняет тесты по `context deadline exceeded`. Runtime и экспортируемый API не изменены.
