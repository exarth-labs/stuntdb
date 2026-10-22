# Database adapters

This folder has one subfolder per database engine. Each adapter implements the interfaces in [`dialect/`](../dialect) and must pass the shared conformance tests.

Database-specific code (SQL text, type names, drivers) belongs here and nowhere else. The linter enforces this.

| Engine | Folder | Status |
|---|---|---|
| SQLite | [`sqlite/`](sqlite) | In development for v0.1 |
| PostgreSQL | `postgres/` | Planned for v0.2 |
| MySQL and MariaDB | `mysql/` | Planned for v0.3 |
| SQL Server | `sqlserver/` | Planned for v1.1 |
| Oracle | `oracle/` | Planned for v1.2 |
