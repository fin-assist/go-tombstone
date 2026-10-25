# Покрытие библиотеки (FIN-243)

Локально нужен Go из `go.mod`, PostgreSQL18 через Docker либо `TEST_DATABASE_URL`, Python3 и jq. Тесты и инструменты в окружении агента запускаются с ограничением времени процесса.

```sh
make test
make cover-unit
make cover-integration
make cover-api
make cover
make lint
make api
```

Каждый коллектор получает пакеты штатным `go list -race ./...`. По явному разрешению владельца исключается только точное имя `<module>/internal/pgtest`: это инфраструктура тестовой БД, а не runtime библиотеки. Другие production-пакеты, включая соседние `pgtest-extra`, вложенные пакеты и код с race build tag, остаются в `-coverpkg`. Список одинаков для Unit, Integration и API. Сами suite продолжают выполнять `./...`, включая тесты helper; профиль не фильтруется анализатором.

`TestUnit`/`FuzzUnit` работают без БД и сети, `TestIntegration`/`FuzzIntegration` проверяют настоящие зависимости. `TestAPI`/`FuzzAPI` во внешнем пакете проверяют публичный Go API с точки зрения потребителя; запись идёт в PostgreSQL-транзакции вызывающего. Исполняемые `Example` входят в API. Race inventory отклоняет неклассифицированные Test/Fuzz до коллектора.

Каждый уровень независимо требует total80 у закреплённого `go-test-coverage/v2 v2.20.1` (принято округление79,95→80,0). Analyzer использует `GOTOOLCHAIN=auto` отдельно от компилятора тестов. Combined строится штатным comma-profile объединением и не компенсирует провал отдельного уровня. JSON/profile guards отклоняют пустые, skipped, failed suite и отсутствующие/пустые/zero-statement профили. Required check `go` сохраняется; отчёты, профили и JSON хранятся в artifacts/summary.

`scripts/test_suite_selection.py` воспроизводит выбор пакетов на настоящем временном Go-модуле, а `test_suite_inputs.py` и `test_coverage_policy.py` проверяют guards и согласованные границы анализатора. Runtime, публичный API и релизы coverage-команды не меняют.
