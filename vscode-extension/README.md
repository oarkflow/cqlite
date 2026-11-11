# cQlite Tools

VS Code extension for `github.com/oarkflow/sqlite` databases.

## Features

- Open a custom SQLite database file such as `complex_embedded.db`.
- Browse tables, views, columns, primary keys, nullability, and indexes.
- Run the selected SQL or the whole active editor.
- Preview the selected table/view immediately from the database editor toolbar.
- Open table/view data in a separate result tab with a row limit.

## Run From This Repository

Install or refresh the extension copy:

```sh
make install-and-reload
```

Or validate it without installing:

```sh
make test-extension
```

1. Open this repository in VS Code.
2. Open `vscode-extension/` as the extension development folder, or use an Extension Development Host with this folder.
3. Keep the default settings:
   - `cQlite.bridgeCommand`: `go`
   - `cQlite.bridgeArgs`: `["run", "./cmd/sqlite-vscode-bridge"]`
4. Run `cQlite: Open Database` and choose a `.db` file, for example:
   - `examples/complex_queries/data/complex_embedded.db`
   - `examples/complex_queries/data/complex_driver.db`

The bridge runs from the workspace root and uses the Go engine directly, so it supports this project’s packed single-file `.db` format.

Opening a `*.db` file in VS Code automatically uses the cQlite custom editor, sets it as the active database, loads schema into the explorer, and shows a toolbar-driven table/view preview.

## Commands

- `cQlite: Open Database`
- `cQlite: Refresh`
- `cQlite: New Query`
- `cQlite: Run Query`
- `Show Table Data`
