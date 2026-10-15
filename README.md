# LocalSyncExt (Windows)

Фоновая синхронизация папок, масок и отдельных файлов между несколькими местами на одном ПК.
Статус: консольное ядро (без GUI/трея). Релизы: вкладка Releases.

Две сборки: `LocalSyncExt.exe` (трей, без окна консоли) и `LocalSyncExt-cli.exe` (консоль).
Запуск: `LocalSyncExt-cli.exe [-config путь] [-once] [-dry-run] [-check] [-tray]`

Опции правила: `mode`, `master`, `conflict`, `notify`, `ignore`, `TrashDays`, `scan`, `recursive`,
`MaxDeletePercent`, `DeleteExtra`, `watch`, `debounce`, `marker`, `nomarker`, `enabled`.
Подробности — в `LocalSyncExt.ini` и `CHANGELOG.md`.
