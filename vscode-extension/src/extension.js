const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const path = require('path');

class SqliteExplorer {
  constructor() {
    this.emitter = new vscode.EventEmitter();
    this.onDidChangeTreeData = this.emitter.event;
    this.schema = null;
    this.root = { kind: 'database' };
  }

  refresh(schema) {
    this.schema = schema;
    this.emitter.fire(this.root);
  }

  getTreeItem(item) {
    if (item.kind === 'database') {
      const db = currentDatabase();
      const label = db ? path.basename(db) : 'Open a database';
      const tree = new vscode.TreeItem(label, db ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.None);
      tree.iconPath = new vscode.ThemeIcon('database');
      tree.description = db || '';
      if (!db) {
        tree.command = { command: 'sujitSqlite.openDatabase', title: 'Open Database' };
      }
      return tree;
    }
    const tree = new vscode.TreeItem(item.label, item.collapsibleState);
    tree.description = item.description;
    tree.tooltip = item.tooltip;
    tree.contextValue = item.contextValue;
    tree.iconPath = item.icon;
    tree.command = item.command;
    return tree;
  }

  getChildren(item) {
    if (!item) {
      return [this.root];
    }
    const objects = schemaObjects(this.schema);
    if (!this.schema || objects.length === 0) {
      return [];
    }
    if (item.kind === 'database') {
      return objects.map(object => ({
        label: object.name,
        description: `${object.type || 'table'} · ${(object.columns || []).length} columns`,
        tooltip: object.sql || object.name,
        contextValue: object.type || 'table',
        icon: new vscode.ThemeIcon(object.type === 'view' ? 'eye' : 'table'),
        collapsibleState: vscode.TreeItemCollapsibleState.Collapsed,
        object
      }));
    }
    if (item.object) {
      return (item.object.columns || []).map(col => ({
        label: col.name,
        description: [col.type, col.primaryKey ? 'PK' : '', col.notNull ? 'NOT NULL' : ''].filter(Boolean).join(' '),
        tooltip: JSON.stringify(col, null, 2),
        icon: new vscode.ThemeIcon(col.primaryKey ? 'key' : 'symbol-field'),
        collapsibleState: vscode.TreeItemCollapsibleState.None,
        column: col
      }));
    }
    return [];
  }
}

class DatabaseEditorProvider {
  constructor(explorer) {
    this.explorer = explorer;
  }

  async openCustomDocument(uri) {
    return { uri, dispose() {} };
  }

  async resolveCustomEditor(document, webviewPanel) {
    const dbPath = document.uri.fsPath;
    webviewPanel.webview.options = { enableScripts: true };
    await setDatabase(dbPath).catch(err => {
      console.warn(`Sujit SQLite: failed to persist database path: ${err && err.message ? err.message : err}`);
    });
    await renderDatabasePanel(this.explorer, webviewPanel, dbPath);
  }
}

async function renderDatabasePanel(explorer, webviewPanel, dbPath, preferredObject = '') {
  const schema = await refreshSchema(explorer, false, dbPath);
  const objects = schemaObjects(schema);
  const selected = objects.some(obj => obj.name === preferredObject) ? preferredObject : (objects[0] ? objects[0].name : '');
  const preview = selected ? await loadObjectRows(dbPath, selected).catch(err => ({ error: err.message })) : null;
  webviewPanel.webview.html = databaseHtml(dbPath, schema, selected, preview);
  webviewPanel.webview.onDidReceiveMessage(async message => handleDatabaseMessage(explorer, webviewPanel, dbPath, message));
}

async function handleDatabaseMessage(explorer, webviewPanel, dbPath, message) {
  if (message.type === 'refresh') {
    const nextSchema = await refreshSchema(explorer, false, dbPath);
    const nextObjects = schemaObjects(nextSchema);
    const nextSelected = nextObjects.some(obj => obj.name === message.selected) ? message.selected : (nextObjects[0] ? nextObjects[0].name : '');
    const nextPreview = nextSelected ? await loadObjectRows(dbPath, nextSelected).catch(err => ({ error: err.message })) : null;
    webviewPanel.webview.html = databaseHtml(dbPath, nextSchema, nextSelected, nextPreview);
  }
  if (message.type === 'select' && message.name) {
    const nextPreview = await loadObjectRows(dbPath, message.name).catch(err => ({ error: err.message }));
    webviewPanel.webview.postMessage({ type: 'preview', name: message.name, sql: defaultObjectSQL(message.name), preview: nextPreview });
  }
  if (message.type === 'runSql') {
    const nextPreview = await runSQLForDatabase(dbPath, message.sql).catch(err => ({ error: err.message, sql: message.sql }));
    webviewPanel.webview.postMessage({ type: 'preview', name: message.name || '', preview: nextPreview });
  }
  if (message.type === 'query') {
    await openSQLEditorPanel(dbPath, message.sql || defaultQuerySQL(), message.name || 'SQL Editor');
  }
  if (message.type === 'open' && message.name) {
    await openDatabaseObjectPanel(explorer, dbPath, message.name);
  }
}

async function openDatabaseObjectPanel(explorer, dbPath, objectName) {
  const title = `Sujit SQLite: ${objectName}`;
  const panel = vscode.window.createWebviewPanel('sujitSqlite.databaseViewerPanel', title, vscode.ViewColumn.Beside, {
    enableScripts: true,
    retainContextWhenHidden: true
  });
  await renderDatabasePanel(explorer, panel, dbPath, objectName);
}

function activate(context) {
  const explorer = new SqliteExplorer();
  const dbEditorProvider = new DatabaseEditorProvider(explorer);
  vscode.window.registerTreeDataProvider('sujitSqliteExplorer', explorer);

  context.subscriptions.push(
    vscode.window.registerCustomEditorProvider('sujitSqlite.databaseViewer', dbEditorProvider, {
      supportsMultipleEditorsPerDocument: false,
      webviewOptions: { retainContextWhenHidden: true }
    }),
    vscode.commands.registerCommand('sujitSqlite.openDatabase', async () => {
      const selected = await vscode.window.showOpenDialog({
        canSelectFiles: true,
        canSelectFolders: true,
        canSelectMany: false,
        filters: { 'Sujit SQLite database': ['db'], 'All files': ['*'] }
      });
      if (!selected || selected.length === 0) {
        return;
      }
      await setDatabase(selected[0].fsPath);
      await refreshSchema(explorer);
    }),
    vscode.commands.registerCommand('sujitSqlite.refresh', async () => refreshSchema(explorer)),
    vscode.commands.registerCommand('sujitSqlite.newQuery', newQuery),
    vscode.commands.registerCommand('sujitSqlite.runQuery', runActiveQuery),
    vscode.commands.registerCommand('sujitSqlite.showTable', async item => {
      const name = item && item.object ? item.object.name : await pickObject(explorer);
      if (name) {
        await showObject(name);
      }
    })
  );

  const db = currentDatabase();
  if (db) {
    refreshSchema(explorer, false, db);
  } else {
    explorer.refresh(null);
  }
}

function deactivate() {}

async function setDatabase(dbPath) {
  if (!dbPath) {
    return;
  }
  const target = vscode.workspace.workspaceFolders && vscode.workspace.workspaceFolders.length > 0
    ? vscode.ConfigurationTarget.Workspace
    : vscode.ConfigurationTarget.Global;
  await vscode.workspace.getConfiguration('sujitSqlite').update('database', dbPath, target);
}

async function refreshSchema(explorer, noisy = true, dbOverride = '') {
  const db = dbOverride || await ensureDatabase(noisy);
  if (!db) {
    explorer.refresh(null);
    return null;
  }
  try {
    const schema = await bridge(['schema'], { db });
    explorer.refresh(schema);
    if (noisy) {
      vscode.window.setStatusBarMessage(`Sujit SQLite: loaded ${schemaObjects(schema).length} objects`, 2500);
    }
    return schema;
  } catch (err) {
    explorer.refresh(null);
    vscode.window.showErrorMessage(`Sujit SQLite schema failed: ${err.message}`);
    return null;
  }
}

async function newQuery() {
  const db = await ensureDatabase();
  if (!db) {
    return;
  }
  await openSQLEditorPanel(db, defaultQuerySQL(), 'SQL Editor');
}

async function openSQLEditorPanel(dbPath, initialSQL, title) {
  const panel = vscode.window.createWebviewPanel('sujitSqliteSqlEditor', `Sujit SQLite: ${title}`, vscode.ViewColumn.Beside, {
    enableScripts: true,
    retainContextWhenHidden: true
  });
  panel.webview.html = sqlEditorHtml(dbPath, initialSQL, null);
  panel.webview.onDidReceiveMessage(async message => {
    if (message.type !== 'runSql') {
      return;
    }
    const nextPreview = await runSQLForDatabase(dbPath, message.sql).catch(err => ({ error: err.message, sql: message.sql }));
    panel.webview.postMessage({ type: 'preview', preview: nextPreview });
  });
}

async function runActiveQuery() {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    vscode.window.showInformationMessage('Open a SQL editor first.');
    return;
  }
  const selection = editor.selection;
  const sql = selection && !selection.isEmpty ? editor.document.getText(selection) : editor.document.getText();
  await runAndShow(sql, 'Query Results');
}

async function showObject(name) {
  const limit = config().get('resultLimit', 200);
  await runAndShow(`SELECT * FROM ${quoteIdent(name)} LIMIT ${limit}`, name);
}

async function loadObjectRows(db, name) {
  return runSQLForDatabase(db, defaultObjectSQL(name));
}

async function runSQLForDatabase(db, sql) {
  const limit = config().get('resultLimit', 200);
  const started = Date.now();
  const result = await bridge(['query', '-sql', sql, '-limit', String(limit)], { db });
  result.latencyMs = Date.now() - started;
  return result;
}

async function runAndShow(sql, title) {
  const db = await ensureDatabase();
  if (!db) {
    return;
  }
  if (!sql || !sql.trim()) {
    vscode.window.showInformationMessage('No SQL to run.');
    return;
  }
  try {
    const result = await runSQLForDatabase(db, sql);
    showResultPanel(title, result);
  } catch (err) {
    vscode.window.showErrorMessage(`Sujit SQLite query failed: ${err.message}`);
  }
}

async function pickObject(explorer) {
  const schema = explorer.schema || await refreshSchema(explorer, false);
  const objects = schemaObjects(schema);
  const picked = await vscode.window.showQuickPick(objects.map(obj => ({
    label: `${obj.type || 'table'}: ${obj.name}`,
    description: `${(obj.columns || []).length} columns`,
    object: obj
  })), { placeHolder: 'Table or view' });
  return picked && picked.object ? picked.object.name : undefined;
}

function showResultPanel(title, result) {
  const panel = vscode.window.createWebviewPanel('sujitSqliteResults', `Sujit SQLite: ${title}`, vscode.ViewColumn.Beside, {});
  panel.webview.html = page('Results', `
    <section class="toolbar"><strong>${esc(resultSummary(result))}</strong></section>
    <pre>${esc(result.sql || '')}</pre>
    ${tableHtml(result)}
  `);
}

function databaseHtml(dbPath, schema, selected, preview) {
  const objects = schemaObjects(schema);
  const options = objects.map(obj => `<button type="button" data-name="${escAttr(obj.name)}"><span>${esc(obj.name)}</span><small>${esc(obj.type || 'table')}</small></button>`).join('');
  const empty = objects.length === 0 ? '<p class="empty">No tables or views found.</p>' : '';
  const previewErrorTemplate = JSON.stringify(errorHtml('__ERROR__'));
  const initialSQL = selected ? defaultObjectSQL(selected) : '';
  const initialPreview = jsJSON(preview || null);
  const objectNames = jsJSON(objects.map(obj => obj.name));
  return page('Database', `
    <section class="app">
      <header class="topbar">
        <label class="object-select" title="${escAttr(dbPath)}">
          <span>Object (${esc(path.basename(dbPath))})</span>
          <div id="objectCombo" class="combo">
            <input id="object" value="${escAttr(selected)}" placeholder="Search objects" autocomplete="off" role="combobox" aria-expanded="false" aria-controls="objectOptions" />
            <div id="objectOptions" class="combo-list" role="listbox" hidden>${options}</div>
          </div>
        </label>
        <div class="actions">
          <button id="refresh" class="icon-button" title="Refresh" aria-label="Refresh">↻</button>
          <button id="open" class="icon-button" title="Open selected object in a new tab" aria-label="Open selected object in a new tab">↗</button>
        </div>
      </header>
      ${empty}
      <section class="sql-editor">
        <div class="editor-shell">
          <textarea id="sql" spellcheck="false">${esc(initialSQL)}</textarea>
          <div class="editor-buttons">
            <button id="execute" class="icon-button" title="Execute SQL" aria-label="Execute SQL">▶</button>
            <button id="query" class="icon-button" title="Open SQL in a separate editor" aria-label="Open SQL in a separate editor">↗</button>
          </div>
        </div>
      </section>
      <main class="preview">
        <nav id="resultTabs" class="result-tabs"></nav>
        <section class="resultbar">
          <div id="response" class="summary"></div>
          <div class="actions export-actions">
            <button id="exportCsv" title="Export rows as CSV" aria-label="Export rows as CSV">CSV</button>
            <button id="exportJson" title="Export rows as JSON" aria-label="Export rows as JSON">JSON</button>
            <button id="exportSql" title="Export rows as SQL INSERT statements" aria-label="Export rows as SQL INSERT statements">SQL</button>
          </div>
        </section>
        <div id="result"></div>
      </main>
    </section>
    <script>
      const vscode = acquireVsCodeApi();
      const object = document.getElementById('object');
      const objectCombo = document.getElementById('objectCombo');
      const objectOptions = document.getElementById('objectOptions');
      const sql = document.getElementById('sql');
      const resultTabs = document.getElementById('resultTabs');
      const response = document.getElementById('response');
      const result = document.getElementById('result');
      const exportActions = document.querySelector('.export-actions');
      const objectNames = ${objectNames};
      let currentResult = ${initialPreview};
      let results = [];
      let activeResult = -1;
      let currentObject = object ? object.value : '';
      document.getElementById('refresh').addEventListener('click', () => vscode.postMessage({ type: 'refresh', selected: object ? object.value : '' }));
      document.getElementById('query').addEventListener('click', () => vscode.postMessage({ type: 'query', name: currentObject, sql: sql.value }));
      document.getElementById('open').addEventListener('click', () => object && vscode.postMessage({ type: 'open', name: object.value }));
      document.getElementById('execute').addEventListener('click', runSql);
      document.getElementById('exportCsv').addEventListener('click', () => exportRows('csv'));
      document.getElementById('exportJson').addEventListener('click', () => exportRows('json'));
      document.getElementById('exportSql').addEventListener('click', () => exportRows('sql'));
      if (object) {
        object.addEventListener('focus', filterObjects);
        object.addEventListener('input', filterObjects);
        object.addEventListener('keydown', event => {
          if (event.key === 'Enter') {
            event.preventDefault();
            selectObject(object.value.trim() || firstVisibleObject());
          } else if (event.key === 'Escape') {
            hideObjectOptions();
          }
        });
        objectOptions.addEventListener('mousedown', event => event.preventDefault());
        objectOptions.addEventListener('click', event => {
          const button = event.target.closest('button[data-name]');
          if (button) selectObject(button.dataset.name);
        });
        document.addEventListener('mousedown', event => {
          if (!objectCombo.contains(event.target)) hideObjectOptions();
        });
      }
      function selectObject(name) {
        if (!objectNames.includes(name)) return;
        object.value = name;
        hideObjectOptions();
        currentObject = name;
        sql.value = defaultSelect(name);
        showLoading();
        vscode.postMessage({ type: 'select', name });
      }
      function filterObjects() {
        const needle = object.value.trim().toLowerCase();
        let anyVisible = false;
        for (const button of objectOptions.querySelectorAll('button[data-name]')) {
          const visible = !needle || button.dataset.name.toLowerCase().includes(needle);
          button.hidden = !visible;
          button.style.display = visible ? 'flex' : 'none';
          anyVisible = anyVisible || visible;
        }
        objectOptions.hidden = !anyVisible;
        object.setAttribute('aria-expanded', anyVisible ? 'true' : 'false');
      }
      function firstVisibleObject() {
        const button = objectOptions.querySelector('button[data-name]:not([hidden])');
        return button ? button.dataset.name : '';
      }
      function hideObjectOptions() {
        objectOptions.hidden = true;
        object.setAttribute('aria-expanded', 'false');
      }
      sql.addEventListener('keydown', event => {
        if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
          event.preventDefault();
          runSql();
        }
      });
      window.addEventListener('message', event => {
        const message = event.data;
        if (message.type !== 'preview') return;
        if (message.name) currentObject = message.name;
        if (message.sql) sql.value = message.sql;
        upsertResult(message.preview || null);
      });
      resultTabs.addEventListener('click', event => {
        const close = event.target.closest('button[data-close]');
        if (close) {
          closeResult(Number(close.dataset.index));
          return;
        }
        const tab = event.target.closest('[data-index]');
        if (!tab) return;
        activeResult = Number(tab.dataset.index);
        currentResult = results[activeResult] ? results[activeResult].data : null;
        renderTabs();
        renderResult(currentResult);
      });
      if (currentResult) {
        upsertResult(currentResult);
      } else {
        renderResult(currentResult);
      }
      function runSql() {
        showLoading();
        vscode.postMessage({ type: 'runSql', name: currentObject, sql: sql.value });
      }
      function showLoading() {
        response.textContent = 'Running...';
        result.innerHTML = '<p class="empty">Loading...</p>';
        updateExportButtons(null);
      }
      function renderResult(data) {
        if (data && data.error) {
          response.textContent = 'Error';
          result.innerHTML = ${previewErrorTemplate}.replace('__ERROR__', escapeHtml(data.error));
          updateExportButtons(null);
          return;
        }
        response.textContent = resultSummary(data);
        result.innerHTML = renderTable(data);
        updateExportButtons(data);
      }
      function upsertResult(data) {
        const key = resultKey(data);
        let index = key ? results.findIndex(entry => entry.key === key) : -1;
        if (index === -1) {
          index = results.length;
          results.push({ key: key || 'result-' + index, data });
        } else {
          results[index].data = data;
        }
        activeResult = index;
        currentResult = data;
        renderTabs();
        renderResult(currentResult);
      }
      function resultKey(data) {
        return data && data.sql ? data.sql.trim().replace(/\\s+/g, ' ') : '';
      }
      function renderTabs() {
        resultTabs.innerHTML = results.map((entry, index) => '<div data-index="' + index + '" class="result-tab ' + (index === activeResult ? 'active' : '') + '" title="' + escapeHtml((entry.data && entry.data.sql) || 'Result') + '"><button type="button" class="tab-main">' + escapeHtml(tabLabel(entry.data, index)) + '</button><button type="button" class="tab-close" data-close="1" data-index="' + index + '" title="Close result">×</button></div>').join('');
      }
      function closeResult(index) {
        results.splice(index, 1);
        if (results.length === 0) {
          activeResult = -1;
          currentResult = null;
          renderTabs();
          renderResult(null);
          return;
        }
        activeResult = Math.min(index, results.length - 1);
        currentResult = results[activeResult].data;
        renderTabs();
        renderResult(currentResult);
      }
      function tabLabel(data, index) {
        const prefix = 'Q' + (index + 1);
        if (data && data.error) return prefix + ' · error';
        if (data && data.columns) return prefix + ' · ' + ((data.rows || []).length) + ' rows';
        if (data) return prefix + ' · ' + (data.rowsAffected || 0) + ' affected';
        return prefix;
      }
      function updateExportButtons(data) {
        const hasRows = !!(data && data.rows && data.rows.length > 0);
        exportActions.hidden = !hasRows;
        exportActions.style.display = hasRows ? 'flex' : 'none';
      }
      function renderTable(result) {
        if (!result || !result.columns || result.columns.length === 0) return '';
        const header = result.columns.map(col => '<th>' + escapeHtml(col) + '</th>').join('');
        const body = (result.rows || []).map(row => '<tr>' + row.map(value => '<td>' + escapeHtml(formatValue(value)) + '</td>').join('') + '</tr>').join('');
        return '<div class="table-wrap"><table><thead><tr>' + header + '</tr></thead><tbody>' + body + '</tbody></table></div>';
      }
      function resultSummary(data) {
        if (!data) return 'No result';
        if (data.columns && data.columns.length > 0) {
          const rows = (data.rows || []).length;
          return rows + ' rows · ' + data.columns.length + ' columns · ' + formatLatency(data.latencyMs);
        }
        return 'Rows affected: ' + (data.rowsAffected || 0) + ' · Last insert id: ' + (data.lastInsertId || 0) + ' · ' + formatLatency(data.latencyMs);
      }
      function formatLatency(value) {
        return typeof value === 'number' ? value + ' ms' : 'latency unavailable';
      }
      function exportRows(kind) {
        if (!currentResult || !currentResult.columns || !currentResult.rows) return;
        const base = currentObject || 'query-results';
        if (kind === 'json') {
          const rows = currentResult.rows.map(row => Object.fromEntries(currentResult.columns.map((col, index) => [col, row[index]])));
          download(base + '.json', JSON.stringify(rows, null, 2), 'application/json');
        } else if (kind === 'csv') {
          download(base + '.csv', toCsv(currentResult), 'text/csv');
        } else {
          download(base + '.sql', toInsertSql(currentResult, currentObject || 'export_rows'), 'application/sql');
        }
      }
      function toCsv(data) {
        const lines = [data.columns.map(csvCell).join(',')];
        for (const row of data.rows || []) lines.push(row.map(csvCell).join(','));
        return lines.join('\\n') + '\\n';
      }
      function csvCell(value) {
        if (value === null || value === undefined) return '';
        const text = typeof value === 'object' ? JSON.stringify(value) : String(value);
        return /[",\\n\\r]/.test(text) ? '"' + text.replace(/"/g, '""') + '"' : text;
      }
      function toInsertSql(data, table) {
        const cols = data.columns.map(quoteIdent).join(', ');
        return (data.rows || []).map(row => 'INSERT INTO ' + quoteIdent(table) + ' (' + cols + ') VALUES (' + row.map(sqlLiteral).join(', ') + ');').join('\\n') + '\\n';
      }
      function sqlLiteral(value) {
        if (value === null || value === undefined) return 'NULL';
        if (typeof value === 'number') return Number.isFinite(value) ? String(value) : 'NULL';
        if (typeof value === 'boolean') return value ? '1' : '0';
        const text = typeof value === 'object' ? JSON.stringify(value) : String(value);
        return "'" + text.replace(/'/g, "''") + "'";
      }
      function download(filename, content, type) {
        const blob = new Blob([content], { type });
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a');
        link.href = url;
        link.download = filename;
        link.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      }
      function defaultSelect(name) {
        return 'SELECT * FROM ' + quoteIdent(name) + ' LIMIT ${Number(config().get('resultLimit', 200))};';
      }
      function quoteIdent(name) {
        return '"' + String(name).replace(/"/g, '""') + '"';
      }
      function formatValue(value) {
        if (value === null || value === undefined) return 'NULL';
        if (typeof value === 'object') return JSON.stringify(value);
        return String(value);
      }
      function escapeHtml(value) {
        return String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      }
    </script>
  `);
}

function sqlEditorHtml(dbPath, initialSQL, preview) {
  const previewErrorTemplate = JSON.stringify(errorHtml('__ERROR__'));
  const initialPreview = jsJSON(preview || null);
  return page('SQL Editor', `
    <section class="app">
      <header class="topbar sql-topbar">
        <div class="object-select" title="${escAttr(dbPath)}">
          <span>SQL Editor (${esc(path.basename(dbPath))})</span>
        </div>
      </header>
      <section class="sql-editor">
        <div class="editor-shell">
          <textarea id="sql" spellcheck="false">${esc(initialSQL)}</textarea>
          <div class="editor-buttons">
            <button id="execute" class="icon-button" title="Execute selected SQL or all SQL" aria-label="Execute selected SQL or all SQL">▶</button>
          </div>
        </div>
      </section>
      <main class="preview">
        <nav id="resultTabs" class="result-tabs"></nav>
        <section class="resultbar">
          <div id="response" class="summary"></div>
          <div class="actions export-actions">
            <button id="exportCsv" title="Export rows as CSV" aria-label="Export rows as CSV">CSV</button>
            <button id="exportJson" title="Export rows as JSON" aria-label="Export rows as JSON">JSON</button>
            <button id="exportSql" title="Export rows as SQL INSERT statements" aria-label="Export rows as SQL INSERT statements">SQL</button>
          </div>
        </section>
        <div id="result"></div>
      </main>
    </section>
    <script>
      const vscode = acquireVsCodeApi();
      const sql = document.getElementById('sql');
      const resultTabs = document.getElementById('resultTabs');
      const response = document.getElementById('response');
      const result = document.getElementById('result');
      const exportActions = document.querySelector('.export-actions');
      let currentResult = ${initialPreview};
      let results = [];
      let activeResult = -1;
      document.getElementById('execute').addEventListener('click', runSql);
      document.getElementById('exportCsv').addEventListener('click', () => exportRows('csv'));
      document.getElementById('exportJson').addEventListener('click', () => exportRows('json'));
      document.getElementById('exportSql').addEventListener('click', () => exportRows('sql'));
      sql.addEventListener('keydown', event => {
        if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
          event.preventDefault();
          runSql();
        }
      });
      window.addEventListener('message', event => {
        const message = event.data;
        if (message.type !== 'preview') return;
        upsertResult(message.preview || null);
      });
      resultTabs.addEventListener('click', event => {
        const close = event.target.closest('button[data-close]');
        if (close) {
          closeResult(Number(close.dataset.index));
          return;
        }
        const tab = event.target.closest('[data-index]');
        if (!tab) return;
        activeResult = Number(tab.dataset.index);
        currentResult = results[activeResult] ? results[activeResult].data : null;
        renderTabs();
        renderResult(currentResult);
      });
      renderResult(currentResult);
      function runSql() {
        const statements = selectedStatements();
        if (statements.length === 0) return;
        response.textContent = statements.length === 1 ? 'Running...' : 'Running ' + statements.length + ' queries...';
        result.innerHTML = '<p class="empty">Loading...</p>';
        updateExportButtons(null);
        for (const statement of statements) {
          vscode.postMessage({ type: 'runSql', sql: statement });
        }
      }
      function selectedStatements() {
        const start = sql.selectionStart;
        const end = sql.selectionEnd;
        const selected = start !== end ? sql.value.slice(start, end).trim() : '';
        if (selected) return [selected];
        return splitStatements(sql.value);
      }
      function splitStatements(text) {
        const statements = [];
        let current = '';
        let quote = '';
        for (let index = 0; index < text.length; index += 1) {
          const char = text[index];
          const next = text[index + 1];
          current += char;
          if (quote) {
            if (char === quote) {
              if (next === quote) {
                current += next;
                index += 1;
              } else {
                quote = '';
              }
            }
            continue;
          }
          if (char === "'" || char === '"') {
            quote = char;
            continue;
          }
          if (char === ';') {
            const statement = current.trim();
            if (statement) statements.push(statement);
            current = '';
          }
        }
        const tail = current.trim();
        if (tail) statements.push(tail);
        return statements;
      }
      function upsertResult(data) {
        const key = resultKey(data);
        let index = key ? results.findIndex(entry => entry.key === key) : -1;
        if (index === -1) {
          index = results.length;
          results.push({ key: key || 'result-' + index, data });
        } else {
          results[index].data = data;
        }
        activeResult = index;
        currentResult = data;
        renderTabs();
        renderResult(currentResult);
      }
      function resultKey(data) {
        return data && data.sql ? data.sql.trim().replace(/\\s+/g, ' ') : '';
      }
      function renderTabs() {
        resultTabs.innerHTML = results.map((entry, index) => '<div data-index="' + index + '" class="result-tab ' + (index === activeResult ? 'active' : '') + '" title="' + escapeHtml((entry.data && entry.data.sql) || 'Result') + '"><button type="button" class="tab-main">' + escapeHtml(tabLabel(entry.data, index)) + '</button><button type="button" class="tab-close" data-close="1" data-index="' + index + '" title="Close result">×</button></div>').join('');
      }
      function closeResult(index) {
        results.splice(index, 1);
        if (results.length === 0) {
          activeResult = -1;
          currentResult = null;
          renderTabs();
          renderResult(null);
          return;
        }
        activeResult = Math.min(index, results.length - 1);
        currentResult = results[activeResult].data;
        renderTabs();
        renderResult(currentResult);
      }
      function tabLabel(data, index) {
        if (data && data.error) return 'Q' + (index + 1) + ' · error';
        if (data && data.columns) return 'Q' + (index + 1) + ' · ' + ((data.rows || []).length) + ' rows';
        if (data) return 'Q' + (index + 1) + ' · ' + (data.rowsAffected || 0) + ' affected';
        return 'Q' + (index + 1);
      }
      function renderResult(data) {
        if (data && data.error) {
          response.textContent = 'Error';
          result.innerHTML = ${previewErrorTemplate}.replace('__ERROR__', escapeHtml(data.error));
          updateExportButtons(null);
          return;
        }
        response.textContent = resultSummary(data);
        result.innerHTML = renderTable(data);
        updateExportButtons(data);
      }
      function updateExportButtons(data) {
        const hasRows = !!(data && data.rows && data.rows.length > 0);
        exportActions.hidden = !hasRows;
        exportActions.style.display = hasRows ? 'flex' : 'none';
      }
      function renderTable(data) {
        if (!data || !data.columns || data.columns.length === 0) return '';
        const header = data.columns.map(col => '<th>' + escapeHtml(col) + '</th>').join('');
        const body = (data.rows || []).map(row => '<tr>' + row.map(value => '<td>' + escapeHtml(formatValue(value)) + '</td>').join('') + '</tr>').join('');
        return '<div class="table-wrap"><table><thead><tr>' + header + '</tr></thead><tbody>' + body + '</tbody></table></div>';
      }
      function resultSummary(data) {
        if (!data) return 'No result';
        if (data.columns && data.columns.length > 0) return (data.rows || []).length + ' rows · ' + data.columns.length + ' columns · ' + formatLatency(data.latencyMs);
        return 'Rows affected: ' + (data.rowsAffected || 0) + ' · Last insert id: ' + (data.lastInsertId || 0) + ' · ' + formatLatency(data.latencyMs);
      }
      function formatLatency(value) {
        return typeof value === 'number' ? value + ' ms' : 'latency unavailable';
      }
      function exportRows(kind) {
        if (!currentResult || !currentResult.columns || !currentResult.rows) return;
        if (kind === 'json') {
          const rows = currentResult.rows.map(row => Object.fromEntries(currentResult.columns.map((col, index) => [col, row[index]])));
          download('query-results.json', JSON.stringify(rows, null, 2), 'application/json');
        } else if (kind === 'csv') {
          download('query-results.csv', toCsv(currentResult), 'text/csv');
        } else {
          download('query-results.sql', toInsertSql(currentResult, 'query_results'), 'application/sql');
        }
      }
      function toCsv(data) {
        const lines = [data.columns.map(csvCell).join(',')];
        for (const row of data.rows || []) lines.push(row.map(csvCell).join(','));
        return lines.join('\\n') + '\\n';
      }
      function csvCell(value) {
        if (value === null || value === undefined) return '';
        const text = typeof value === 'object' ? JSON.stringify(value) : String(value);
        return /[",\\n\\r]/.test(text) ? '"' + text.replace(/"/g, '""') + '"' : text;
      }
      function toInsertSql(data, table) {
        const cols = data.columns.map(quoteIdent).join(', ');
        return (data.rows || []).map(row => 'INSERT INTO ' + quoteIdent(table) + ' (' + cols + ') VALUES (' + row.map(sqlLiteral).join(', ') + ');').join('\\n') + '\\n';
      }
      function sqlLiteral(value) {
        if (value === null || value === undefined) return 'NULL';
        if (typeof value === 'number') return Number.isFinite(value) ? String(value) : 'NULL';
        if (typeof value === 'boolean') return value ? '1' : '0';
        const text = typeof value === 'object' ? JSON.stringify(value) : String(value);
        return "'" + text.replace(/'/g, "''") + "'";
      }
      function quoteIdent(name) {
        return '"' + String(name).replace(/"/g, '""') + '"';
      }
      function download(filename, content, type) {
        const blob = new Blob([content], { type });
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a');
        link.href = url;
        link.download = filename;
        link.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      }
      function formatValue(value) {
        if (value === null || value === undefined) return 'NULL';
        if (typeof value === 'object') return JSON.stringify(value);
        return String(value);
      }
      function escapeHtml(value) {
        return String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      }
    </script>
  `);
}

function tableHtml(result) {
  if (!result || !result.columns || result.columns.length === 0) {
    return result ? `<section class="summary"><strong>${esc(resultSummary(result))}</strong></section>` : '';
  }
  const header = result.columns.map(col => `<th>${esc(col)}</th>`).join('');
  const body = (result.rows || []).map(row => `<tr>${row.map(value => `<td>${esc(formatValue(value))}</td>`).join('')}</tr>`).join('');
  return `<section class="summary"><strong>${esc(resultSummary(result))}</strong></section><div class="table-wrap"><table><thead><tr>${header}</tr></thead><tbody>${body}</tbody></table></div>`;
}

function errorHtml(message) {
  return `<p class="error">${esc(message)}</p>`;
}

function resultSummary(result) {
  if (!result) {
    return 'No rows';
  }
  if (result.columns && result.columns.length > 0) {
    return `${(result.rows || []).length} rows`;
  }
  return `Rows affected: ${result.rowsAffected || 0}, last insert id: ${result.lastInsertId || 0}`;
}

function page(title, body) {
  return `<!doctype html>
  <html>
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>${esc(title)}</title>
    <style>
      body { color: var(--vscode-foreground); background: var(--vscode-editor-background); font-family: var(--vscode-font-family); margin: 0; }
      .app { min-height: 100vh; }
      .topbar { position: sticky; top: 0; z-index: 3; display: grid; grid-template-columns: minmax(360px, 1fr) auto; gap: 12px; align-items: end; padding: 12px; border-bottom: 1px solid var(--vscode-panel-border); background: var(--vscode-editor-background); }
      .sql-topbar { grid-template-columns: 1fr; }
      .object-select { display: grid; gap: 4px; }
      .object-select span { color: var(--vscode-descriptionForeground); font-size: 12px; }
      .combo { position: relative; }
      .combo input { width: 100%; box-sizing: border-box; }
      .combo-list { position: absolute; left: 0; right: 0; top: calc(100% + 2px); z-index: 20; max-height: 240px; overflow-y: auto; border: 1px solid var(--vscode-dropdown-border); background: var(--vscode-dropdown-background); box-shadow: 0 8px 20px rgba(0, 0, 0, 0.28); }
      .combo-list button { width: 100%; display: flex; justify-content: space-between; gap: 12px; align-items: center; border: 0; border-radius: 0; color: var(--vscode-dropdown-foreground); background: transparent; text-align: left; }
      .combo-list button[hidden] { display: none; }
      .combo-list button:hover, .combo-list button:focus { background: var(--vscode-list-hoverBackground); outline: none; }
      .combo-list small { color: var(--vscode-descriptionForeground); }
      .actions, .toolbar { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
      button, input, select, textarea { font: inherit; color: var(--vscode-input-foreground); background: var(--vscode-input-background); border: 1px solid var(--vscode-input-border); border-radius: 4px; padding: 6px 8px; }
      button { cursor: pointer; color: var(--vscode-button-foreground); background: var(--vscode-button-background); border-color: var(--vscode-button-border); }
      .icon-button { width: 32px; height: 32px; display: inline-grid; place-items: center; padding: 0; font-size: 15px; line-height: 1; }
      textarea { width: 100%; height: 76px; min-height: 64px; resize: vertical; box-sizing: border-box; font-family: var(--vscode-editor-font-family); line-height: 1.45; padding-right: 82px; }
      .sql-editor { padding: 12px; border-bottom: 1px solid var(--vscode-panel-border); }
      .editor-shell { position: relative; }
      .editor-buttons { position: absolute; top: 8px; right: 8px; display: flex; gap: 6px; }
      .preview, .toolbar { padding: 12px; }
      .summary { color: var(--vscode-descriptionForeground); margin-bottom: 8px; min-height: 20px; }
      .resultbar { display: flex; justify-content: space-between; gap: 12px; align-items: center; margin-bottom: 8px; }
      .resultbar .summary { margin-bottom: 0; }
      .export-actions { flex-shrink: 0; }
      .result-tabs { display: flex; gap: 4px; overflow-x: auto; margin-bottom: 8px; }
      .result-tab { flex: 0 0 auto; display: inline-flex; align-items: center; max-width: 220px; border: 1px solid var(--vscode-panel-border); border-bottom-color: transparent; border-radius: 4px 4px 0 0; background: var(--vscode-tab-inactiveBackground); }
      .result-tab.active { background: var(--vscode-tab-activeBackground); color: var(--vscode-tab-activeForeground); }
      .result-tab button { border: 0; border-radius: 0; color: inherit; background: transparent; }
      .result-tab .tab-main { min-width: 0; max-width: 178px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
      .result-tab .tab-close { width: 26px; padding: 4px; opacity: 0.78; }
      .result-tab .tab-close:hover { opacity: 1; background: var(--vscode-toolbar-hoverBackground); }
      .table-wrap { overflow: auto; border: 1px solid var(--vscode-panel-border); }
      table { border-collapse: collapse; width: 100%; font-size: 12px; }
      th, td { border-bottom: 1px solid var(--vscode-panel-border); border-right: 1px solid var(--vscode-panel-border); padding: 6px 8px; text-align: left; vertical-align: top; white-space: nowrap; }
      th { position: sticky; top: 0; background: var(--vscode-editorWidget-background); z-index: 1; }
      td { max-width: 420px; overflow: hidden; text-overflow: ellipsis; }
      pre { margin: 12px; background: var(--vscode-textCodeBlock-background); padding: 10px; overflow: auto; border-radius: 4px; }
      .empty { padding: 12px; color: var(--vscode-descriptionForeground); }
      .error { color: var(--vscode-errorForeground); }
      @media (max-width: 760px) { .topbar { grid-template-columns: 1fr; align-items: stretch; } .resultbar { align-items: flex-start; flex-direction: column; } }
    </style>
  </head>
  <body>${body}</body>
  </html>`;
}

async function ensureDatabase(prompt = true) {
  const configured = currentDatabase();
  if (configured) {
    return configured;
  }
  if (!prompt) {
    return '';
  }
  await vscode.commands.executeCommand('sujitSqlite.openDatabase');
  return currentDatabase();
}

function schemaObjects(schema) {
  if (!schema) {
    return [];
  }
  const objects = schema.objects || schema.tables || [];
  return objects.slice().sort((a, b) => {
    if ((a.type || 'table') !== (b.type || 'table')) {
      return (a.type || 'table') === 'table' ? -1 : 1;
    }
    return a.name.localeCompare(b.name);
  });
}

function currentDatabase() {
  return config().get('database', '');
}

function config() {
  return vscode.workspace.getConfiguration('sujitSqlite');
}

function bridge(args, options) {
  const cfg = config();
  const db = options.db;
  const cwd = workspaceRoot();
  const bridgeCommand = resolveBridgeCommand(cfg, cwd);
  const command = bridgeCommand.command;
  const prefixArgs = bridgeCommand.prefixArgs;
  const commandArgs = [];
  const flagArgs = [];
  for (let i = 0; i < args.length; i += 1) {
    if (args[i].startsWith('-')) {
      flagArgs.push(args[i], args[i + 1]);
      i += 1;
    } else {
      commandArgs.push(args[i]);
    }
  }
  const fullArgs = [...prefixArgs, '-db', db, ...flagArgs, ...commandArgs];
  return new Promise((resolve, reject) => {
    cp.execFile(command, fullArgs, { cwd, maxBuffer: 1024 * 1024 * 16 }, (err, stdout, stderr) => {
      if (err) {
        reject(new Error((stderr || stdout || err.message).trim()));
        return;
      }
      try {
        const parsed = JSON.parse(stdout);
        if (!parsed.ok) {
          reject(new Error(parsed.error || 'bridge returned an error'));
          return;
        }
        resolve(parsed);
      } catch (parseErr) {
        reject(new Error(`invalid bridge response: ${parseErr.message}`));
      }
    });
  });
}

function resolveBridgeCommand(cfg, cwd) {
  const defaultArgs = ['run', './cmd/sqlite-vscode-bridge'];
  const configuredCommand = cfg.get('bridgeCommand', 'go');
  const configuredArgs = cfg.get('bridgeArgs', defaultArgs);
  if (configuredCommand === 'go' && sameStringList(configuredArgs, defaultArgs)) {
    const binary = path.join(cwd, 'bin', process.platform === 'win32' ? 'sqlite-vscode-bridge.exe' : 'sqlite-vscode-bridge');
    if (fs.existsSync(binary)) {
      return { command: binary, prefixArgs: [] };
    }
  }
  return { command: configuredCommand, prefixArgs: configuredArgs };
}

function sameStringList(a, b) {
  return Array.isArray(a) && Array.isArray(b) && a.length === b.length && a.every((value, index) => value === b[index]);
}

function workspaceRoot() {
  const folders = vscode.workspace.workspaceFolders;
  if (!folders || folders.length === 0) {
    return process.cwd();
  }
  return folders[0].uri.fsPath;
}

function quoteIdent(name) {
  return `"${String(name).replace(/"/g, '""')}"`;
}

function defaultObjectSQL(name) {
  return `SELECT * FROM ${quoteIdent(name)} LIMIT ${Number(config().get('resultLimit', 200))};`;
}

function defaultQuerySQL() {
  return [
    'SELECT name, sql',
    'FROM sqlite_schema',
    "WHERE type IN ('table', 'view')",
    'ORDER BY name;'
  ].join('\n');
}

function formatValue(value) {
  if (value === null || value === undefined) {
    return 'NULL';
  }
  if (typeof value === 'object') {
    return JSON.stringify(value);
  }
  return String(value);
}

function esc(value) {
  return String(value)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function escAttr(value) {
  return esc(value).replace(/'/g, '&#39;');
}

function jsJSON(value) {
  return JSON.stringify(value).replace(/</g, '\\u003c');
}

module.exports = { activate, deactivate };
