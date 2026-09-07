<a id="top"></a>
# Глоссарий

## Оглавление

1. [Термины](#terms)
2. [Идентификаторы программы](#program-ids)

<a id="terms"></a>
## 1. Термины

| Термин | Расшифровка и значение |
|---|---|
| ADR | Architecture Decision Record — запись архитектурного решения |
| API | Application Programming Interface — программный интерфейс |
| AST | Abstract Syntax Tree — абстрактное синтаксическое дерево Go-кода |
| Baseline | точный список существующего временно допустимого долга |
| Bounded join | гарантированное ожидание завершения с конечной границей времени/условия |
| Characterization test | тест, фиксирующий текущее наблюдаемое поведение перед refactor |
| CI | Continuous Integration — непрерывная интеграция; hosted CI для обычных PR в этой программе не используется |
| CLI | Command-Line Interface — интерфейс командной строки |
| Composition root | единственное место создания и связывания production-зависимостей |
| DI | Dependency Injection — внедрение зависимостей экземпляра |
| Fail closed | безопасный отказ при ошибке или недоказанности |
| Finding | результат аудита `R-*` |
| GUI | Graphical User Interface — графический интерфейс |
| IPC | Inter-Process Communication — межпроцессное взаимодействие |
| JSON | JavaScript Object Notation — текстовый формат структурированных данных |
| HTTP | Hypertext Transfer Protocol — протокол передачи гипертекста |
| JSON-RPC | Remote Procedure Call поверх JSON |
| LSP | Language Server Protocol — протокол языкового сервера |
| MCP | Model Context Protocol — протокол контекста модели |
| OIDC | OpenID Connect — протокол федеративной идентификации, используемый для trusted publishing |
| OSV | Open Source Vulnerabilities — формат и база уязвимостей открытого ПО |
| Owner | единственный ответственный владелец инварианта или lifecycle |
| P0/P1/P2/P3 | уровни приоритета: блокирующий, высокий, существенный и плановый |
| PID | Process Identifier — идентификатор процесса |
| PR | Pull Request — запрос на включение изменений |
| Ratchet | механизм, допускающий точный старый долг, но запрещающий новый и выросший |
| SBOM | Software Bill of Materials — перечень компонентов программного продукта |
| SHA-256 | Secure Hash Algorithm 256-bit — 256-битный криптографический хеш |
| SSE | Server-Sent Events — поток событий от сервера к клиенту поверх HTTP |
| State machine | конечный автомат состояний и переходов |
| TDD | Test-Driven Development — разработка через падающий тест, реализацию и повторную проверку |
| Worker | фоновая goroutine или процесс с contract owner/cancel/join/bound |
| YAML | YAML Ain't Markup Language — человекочитаемый формат сериализации |

<a id="program-ids"></a>
## 2. Идентификаторы программы

| Формат | Значение |
|---|---|
| `R-01` | finding аудита |
| `ARCH-001`, `SEC-001` | нормативное требование |
| `AT-001` | архитектурный приёмочный тест |
| `WP-11A` | рабочий пакет |
| `PR-11A-01` | нормативный идентификатор Pull Request |
| `D-001` | решение программы |
| `ADR-0001` | запись принятого архитектурного решения |

[Вернуться к началу](#top)
