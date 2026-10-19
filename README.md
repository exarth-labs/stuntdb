# stuntdb

**A stunt double for your database.** stuntdb makes a small, safe copy of a production database. It replaces personal data with realistic fake values and keeps every relationship between tables intact, so developers and AI agents can work with data that looks and behaves like the real thing.

> **Status: early development.** stuntdb doesn't do anything useful yet.
>
> - The first release with real functionality will support **SQLite**.
> - **PostgreSQL**, **MySQL / MariaDB**, **SQL Server** and **Oracle** will follow, one per release.

## What it will do

- **Subset:** copy only a slice of a database, for example 5% of customers, while keeping every foreign key intact.
- **Mask:** replace names, emails, phone numbers and secrets with consistent fake values, where the same input always gets the same fake output.
- **Stay safe:** never write to the source database, and refuse to run while any column is still unreviewed.
- **Share:** write one `.stuntdb` snapshot file that can be restored on any developer's machine.

## Building from source

You need [Go](https://go.dev/dl/) 1.26 or newer and [Task](https://taskfile.dev).

```sh
git clone https://github.com/exarth-labs/stuntdb.git
cd stuntdb
task build            # creates bin/stuntdb (bin/stuntdb.exe on Windows)
./bin/stuntdb version
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), and please follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please report security problems privately. See [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE)
