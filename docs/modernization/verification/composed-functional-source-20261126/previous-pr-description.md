## Source5 и full11 — сохранённая контрольная точка

**Полная приёмка открыта.** В [едином плане](https://github.com/applicate2628/mcp-local-hub/blob/8d1dda187e90df78867f0097d7ec9f6b0442e58c/.plans/2026-11/open-pr-integration-20261117.md#source5-full11-checkpoint-20261126) и [пакете доказательств](https://github.com/applicate2628/mcp-local-hub/blob/8d1dda187e90df78867f0097d7ec9f6b0442e58c/docs/modernization/verification/cache-owner-custody-20261126/source5-full11/README.md) зафиксированы материализация Source5, 13 тестовых изменений, DATA/composition и перенос пяти закрытых ресурсов в trash.

Full11: build и vet — PASS; стандартный тестовый запуск — FAIL с исходным владением UNKNOWN; тегированная команда — NOT_RUN. Терминальное завершение исходного родителя ещё не принято. Source5, выводы и кеши full11 удерживаются. Подтверждён дефект внешней named/deferred-ветки: timeout через subprocess.run прекращает непосредственный controller до приёмки вложенного native/IO; исправление удержания исходных объектов проходит отдельную квалификацию. Это не меняет результат full11 и не восстанавливает потерянное владение.

Ниже сохранена параллельная квалификация B. Слияний, изменений установленного продукта и платных запусков не было; внешнее ревью точной вершины — REVIEW_PENDING.

---

## Квалификация B — 26 ноября 2026 года

**Черновик. Полная приёмка остаётся открытой.** Source-v5 восстановил Product `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`. Из него выдан Test Source `001648f44bf6ef0191f1e21d92f1aecf2a2a3022` с 13 тестовыми изменениями.

- [Сверка исходников и полных прогонов](https://github.com/applicate2628/mcp-local-hub/blob/b0719bcc4c95d1af0a6f509a54607200a7f55b7e/docs/modernization/verification/cache-owner-custody-20261126/source-v5-reconciliation-20261126.md). Полный v10 сохраняет FAIL. В v11 сборка и vet прошли, исходное владение стандартного прогона осталось `UNKNOWN`, тегированная команда не выдана. Родительская терминальная квитанция на момент проверки ещё не получена.
- [Исполнение Go reader на Linux/amd64](https://github.com/applicate2628/mcp-local-hub/blob/b0719bcc4c95d1af0a6f509a54607200a7f55b7e/docs/modernization/verification/cache-owner-custody-20261126/posix-runtime-20261126.md). На точной вершине #668 по 5 корневых тестов и 15 подтестов прошли в обоих режимах.
- [Нативный Windows recovery namespace](https://github.com/applicate2628/mcp-local-hub/blob/b0719bcc4c95d1af0a6f509a54607200a7f55b7e/docs/modernization/verification/cache-owner-custody-20261126/powershell-namespace-20261126.md). На точном Test Source прошли три сценария с проверенной временной копией PowerShell 7 и каноническим путём Go.

Открыты обе полные команды, Unix/ARM64, текущий жизненный цикл Product/Serena и внешнее ревью точной вершины.

[Полное предыдущее описание и история](https://github.com/applicate2628/mcp-local-hub/blob/b0719bcc4c95d1af0a6f509a54607200a7f55b7e/docs/modernization/verification/cache-owner-custody-20261126/previous-pr-description-before-b-20261126.md)

<!-- functional-route-20261126:start -->
### Функциональный маршрут — 26 ноября 2026

[Единый действующий §3](https://github.com/applicate2628/mcp-local-hub/blob/0c255490461fed0a3ee3d43acdd5216e2e064a53/.plans/2026-11/open-pr-integration-20261117.md#3-порядок-выполнения): функциональные причины → одна композиция → реальные сценарии → одна полная кампания → точное ревью и публикация. Оставшееся усиление защиты и экспериментальная работа сохранены отдельным хвостом. План аудита ссылается на этот порядок; его собственные критерии и исторические результаты сохранены. Уже квалифицированные POSIX, Reader matrix и namespace не запускаются повторно без изменённой границы.

Этот commit меняет только два документа. Полные тесты, Product qualification и внешнее ревью новых голов им не закрываются. PR остаётся Draft; слияний нет.
<!-- functional-route-20261126:end -->