# cQlite Tools

VS Code extension for `github.com/oarkflow/cqlite` databases.

## Features

- Open `.cq` files directly into a table-first workbench with an editable **Editor** view, an inline SQL editor above the data grid, and a persistent, searchable Tables and Views sidebar.
- Browse object structure, columns, primary keys, nullability, indexes, foreign keys, and triggers.
- Browse, filter, sort, and page through table data. Cell edits save when you leave the cell; press Enter to finish an edit or Escape to restore its previous value. Add and delete rows, then use **Save changes** to commit those staged operations.
- Right-click a cell or press `Shift+F10` to set SQL `NULL`, restore that column’s declared default, or enter a SQL expression such as `now()`. SQL `NULL` cells are labeled and styled distinctly from empty text.
- Run one or more SQL statements in the workbench’s syntax-highlighted inline editor with **Run** or `Ctrl/Cmd+Enter`; Tab indents by two spaces. Result tabs and CSV, JSON, and SQL exports appear above the editable table grid. Native SQL documents also have a Run action in the editor title bar; running them opens the `.cq` workbench if needed, then shows results there instead of opening a separate results tab.
- Export query results and table data as CSV, JSON, or SQL. Review, search, rerun, and export query history.
- Add, rename, and drop columns; create tables and views; and create or drop multi-column and unique indexes from Manage. Review generated SQL before applying it, or enter schema SQL directly.
- Open the bundled example database from the Command Palette or the cQlite Explorer toolbar.

## Included Example Database

Open the bundled [`complex_embedded.cq`](examples/complex_embedded.cq) directly in VS Code, or run `cQlite: Open Example Database` from the Command Palette or cQlite Explorer toolbar. The command uses the repository example when it is available and otherwise copies the bundled file into writable extension storage before opening it. It includes accounts, orders, settings tables, an index, an `active_orders` view, and seeded rows for browsing and query examples.

Regenerate the committed example from its source with:

```sh
go run ./examples/complex_queries
```

## Run From This Repository

Install or refresh the extension copy:

```sh
make install-and-reload
```

Or validate it without installing:

```sh
make test-extension
```

Build a platform-specific VSIX containing the current bridge and bundled example:

```sh
make package-extension
```

The VSIX is written to `vscode-extension/` and can be installed with VS Code’s **Install from VSIX…** command.

1. Open this repository in VS Code.
2. Open `vscode-extension/` as the extension development folder, or use an Extension Development Host with this folder.
3. Keep the default settings:
   - `cQlite.bridgeCommand`: `go`
   - `cQlite.bridgeArgs`: `["run", "./cmd/cqlite-vscode-bridge"]`
4. Run `cQlite: Open Example Database`, or use `cQlite: Open Database` and choose a `.cq` file, for example:
   - `examples/complex_queries/data/complex_embedded.cq`
   - `examples/complex_queries/data/complex_driver.cq`

The bridge runs from the workspace root and uses the Go engine directly, so it supports this project’s packed single-file `.cq` format.

Opening a `*.cq` file in VS Code automatically uses the cQlite custom editor, sets it as the active database, loads schema into the explorer, and shows a toolbar-driven table/view preview.

## Commands

- `cQlite: Open Database`
- `cQlite: Refresh`
- `cQlite: New Query`
- `cQlite: Run Query`
- `Show Table Data`
