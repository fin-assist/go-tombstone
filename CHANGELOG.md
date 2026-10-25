# Изменения

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии и выпуск — [docs/releasing.md](docs/releasing.md). Каждый PR, который меняет API или поведение библиотеки, добавляет строку в «Unreleased»; при выпуске раздел получает номер версии и дату.

## [Unreleased]

### Изменено

- FIN-319: `internal/pgtest` больше не копирует go-migrate (по FIN-310): оставлен только выбор тестового PostgreSQL (`TEST_DATABASE_URL`/testcontainers, skip локально, ошибка в CI), создание БД, правила файлов и обход up → down → up, а также применение источника в usage-тестах (`Apply`) делегированы `github.com/fin-assist/go-migrate` v0.1.1. go-migrate импортируется только тестами, но как require модуля входит в граф модулей и повышает минимальную версию go-migrate у сервисов, импортирующих библиотеку (сервисы уже на v0.1.1). Отрицательный интеграционный тест закрепляет, что `Apply` завершает тест при ошибке `migrate.Up`. Исключение `internal/pgtest` из покрытия не изменилось; runtime и экспортируемый API не менялись.
- FIN-243: независимые unit/integration/API statements gates 80 % production-пакетов (по согласованию исключён только internal/pgtest), защита от пустых suite и содержательные тесты публичного API; прежний coverage-diff удалён. Runtime и экспортируемый API не изменены.
- FIN-312: записи, вошедшие в уже выпущенный тег v0.1.0, перенесены из «Unreleased» в секцию v0.1.0; в docs/releasing.md записано исключение для GitHub Release v0.1.0. Runtime, API и теги не изменены.

### Добавлено

- FIN-244: пример и public-consumer проверки композиции с опубликованным go-postgres v0.1.2: единая сервисная транзакция, откат при ошибке callback/COMMIT, вложенные и чужие контексты, конкуренция Guard/Purge и освобождение соединения после panic/отмены/Goexit; production API и схема не изменены.

## [v0.1.0] — 2026-10-23

### Изменено

- FIN-295: README «Внедрение» описывает подключение через go-grpc `server.Tombstones`: адаптер над `Store.Deleted` без кэша, отличие кода сбоя (`UNAVAILABLE`), учёт метрики при `AllowDeleted`. Runtime и экспортируемый API не изменены.
- FIN-214: правило Squawk prefer-bigint-over-int отключено для репозитория; размер целочисленного типа выбирается по диапазону домена.

### Добавлено

- FIN-110: tombstone удалённых пользователей, атомарный PurgeUserData, unary/stream TOKEN_REVOKED, транзакционная защита Guard от конкурентного воскрешения и метрика отказов; миграции и инструкция внедрения. UUID нормализуется для блокировки, tombstone и callback удаления; отмены и дедлайны сохраняют gRPC-коды.
