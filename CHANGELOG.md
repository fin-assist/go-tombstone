# Изменения

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии и выпуск — [docs/releasing.md](docs/releasing.md). Каждый PR, который меняет API или поведение библиотеки, добавляет строку в «Unreleased»; при выпуске раздел получает номер версии и дату.

## [Unreleased]

### Добавлено

- FIN-110: tombstone удалённых пользователей, атомарный PurgeUserData, unary/stream TOKEN_REVOKED, транзакционная защита Guard от конкурентного воскрешения и метрика отказов; миграции и инструкция внедрения.
