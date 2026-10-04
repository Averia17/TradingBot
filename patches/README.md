# Historical patch archive

`trading-agents/windows-test-isolation.patch` относится к первоначальной M1
проверке Windows suite. После перехода на clean upstream + adapters этот patch
не входит в `upstream.lock.json` и не применяется bootstrap/updater.
Runtime TradingAgents не менялся этим patch. Историческая локальная Git ветка
`bot/integration` сохранена, рабочий checkout возвращён к официальному source SHA.

Текущая политика и проверки: [upstream updates](../docs/upstream-updates.md).
