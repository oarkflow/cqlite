const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const path = require('path');

let extensionContext;

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
        tree.command = { command: 'cQlite.openDatabase', title: 'Open Database' };
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
      console.warn(`cQlite: failed to persist database path: ${err && err.message ? err.message : err}`);
    });
    await renderDatabasePanel(this.explorer, webviewPanel, dbPath);
  }
}

async function renderDatabasePanel(explorer, webviewPanel, dbPath, preferredObject = '') {
  const schema = await refreshSchema(explorer, false, dbPath);
  const objects = schemaObjects(schema);
  const selected = objects.some(obj => obj.name === preferredObject) ? preferredObject : (objects[0] ? objects[0].name : '');
  const preview = selected ? await loadObjectRows(dbPath, selected).catch(err => ({ error: err.message })) : null;
  webviewPanel.webview.html = databaseHtml(dbPath, schema, selected, preview, queryLogs(dbPath));
  webviewPanel.webview.onDidReceiveMessage(async message => handleDatabaseMessage(explorer, webviewPanel, dbPath, message));
}

async function handleDatabaseMessage(explorer, webviewPanel, dbPath, message) {
  if (message.type === 'refresh') {
    const nextSchema = await refreshSchema(explorer, false, dbPath);
    const nextObjects = schemaObjects(nextSchema);
    const nextSelected = nextObjects.some(obj => obj.name === message.selected) ? message.selected : (nextObjects[0] ? nextObjects[0].name : '');
    const nextPreview = nextSelected ? await loadObjectRows(dbPath, nextSelected).catch(err => ({ error: err.message })) : null;
    webviewPanel.webview.html = databaseHtml(dbPath, nextSchema, nextSelected, nextPreview, queryLogs(dbPath));
  }
  if (message.type === 'select' && message.name) {
    const nextPreview = await loadObjectRows(dbPath, message.name).catch(err => ({ error: err.message }));
    webviewPanel.webview.postMessage({ type: 'preview', name: message.name, sql: defaultObjectSQL(message.name), preview: nextPreview });
  }
  if (message.type === 'runSql') {
    const nextPreview = await runSQLForDatabase(dbPath, message.sql, message.source || 'query').catch(async err => {
      const failed = { error: err.message, sql: message.sql };
      await appendQueryLog(dbPath, failed, message.source || 'query');
      return failed;
    });
    webviewPanel.webview.postMessage({ type: 'preview', name: message.name || '', preview: nextPreview });
  }
  if (message.type === 'applyStatements') {
    const applied = await applyStatements(dbPath, message.command || 'applyDataEdits', message.statements || [], message.previewSql || [], message.source || 'manage').catch(async err => {
      const failed = { error: err.message, sql: (message.statements || []).join('\n') };
      await appendQueryLog(dbPath, failed, message.source || 'manage');
      return failed;
    });
    webviewPanel.webview.postMessage({ type: 'applied', preview: applied, logs: queryLogs(dbPath) });
  }
  if (message.type === 'logs') {
    webviewPanel.webview.postMessage({ type: 'logs', logs: queryLogs(dbPath) });
  }
  if (message.type === 'clearLogs') {
    await clearQueryLogs(dbPath);
    webviewPanel.webview.postMessage({ type: 'logs', logs: [] });
  }
  if (message.type === 'query') {
    await openSQLEditorPanel(dbPath, message.sql || defaultQuerySQL(), message.name || 'SQL Editor');
  }
  if (message.type === 'open' && message.name) {
    await openDatabaseObjectPanel(explorer, dbPath, message.name);
  }
}

async function openDatabaseObjectPanel(explorer, dbPath, objectName) {
  const title = `cQlite: ${objectName}`;
  const panel = vscode.window.createWebviewPanel('cQlite.databaseViewerPanel', title, vscode.ViewColumn.Beside, {
    enableScripts: true,
    retainContextWhenHidden: true
  });
  await renderDatabasePanel(explorer, panel, dbPath, objectName);
}

function activate(context) {
  extensionContext = context;
  const explorer = new SqliteExplorer();
  const dbEditorProvider = new DatabaseEditorProvider(explorer);
  vscode.window.registerTreeDataProvider('cQliteExplorer', explorer);

  context.subscriptions.push(
    vscode.window.registerCustomEditorProvider('cQlite.databaseViewer', dbEditorProvider, {
      supportsMultipleEditorsPerDocument: false,
      webviewOptions: { retainContextWhenHidden: true }
    }),
    vscode.commands.registerCommand('cQlite.openDatabase', async () => {
      const selected = await vscode.window.showOpenDialog({
        canSelectFiles: true,
        canSelectFolders: true,
        canSelectMany: false,
        filters: { 'cQlite database': ['db'], 'All files': ['*'] }
      });
      if (!selected || selected.length === 0) {
        return;
      }
      await setDatabase(selected[0].fsPath);
      await refreshSchema(explorer);
    }),
    vscode.commands.registerCommand('cQlite.refresh', async () => refreshSchema(explorer)),
    vscode.commands.registerCommand('cQlite.newQuery', newQuery),
    vscode.commands.registerCommand('cQlite.runQuery', runActiveQuery),
    vscode.commands.registerCommand('cQlite.showTable', async item => {
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
  await vscode.workspace.getConfiguration('cQlite').update('database', dbPath, target);
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
      vscode.window.setStatusBarMessage(`cQlite: loaded ${schemaObjects(schema).length} objects`, 2500);
    }
    return schema;
  } catch (err) {
    explorer.refresh(null);
    vscode.window.showErrorMessage(`cQlite schema failed: ${err.message}`);
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
  const panel = vscode.window.createWebviewPanel('cQliteSqlEditor', `cQlite: ${title}`, vscode.ViewColumn.Beside, {
    enableScripts: true,
    retainContextWhenHidden: true
  });
  panel.webview.html = sqlEditorHtml(dbPath, initialSQL, null);
  panel.webview.onDidReceiveMessage(async message => {
    if (message.type !== 'runSql') {
      return;
    }
    const nextPreview = await runSQLForDatabase(dbPath, message.sql, 'sql-editor').catch(async err => {
      const failed = { error: err.message, sql: message.sql };
      await appendQueryLog(dbPath, failed, 'sql-editor');
      return failed;
    });
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
  const limit = config().get('resultLimit', 200);
  const result = await bridge(['browseRows', '-table', name, '-limit', String(limit)], { db });
  result.latencyMs = typeof result.durationMs === 'number' ? result.durationMs : 0;
  return result;
}

async function runSQLForDatabase(db, sql, source = '') {
  const limit = config().get('resultLimit', 200);
  const started = Date.now();
  const result = await bridge(['execute', '-sql', sql, '-limit', String(limit)], { db });
  result.latencyMs = typeof result.durationMs === 'number' ? result.durationMs : 0;
  if (source) {
    await appendQueryLog(db, result, source);
  }
  return result;
}

async function applyStatements(db, command, statements, previewSql, source) {
  const started = Date.now();
  const payload = JSON.stringify({ statements, previewSql });
  const result = await bridge([command, '-payload', payload], { db });
  result.sql = statements.join('\n');
  result.latencyMs = typeof result.durationMs === 'number' ? result.durationMs : Date.now() - started;
  await appendQueryLog(db, result, source);
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
    vscode.window.showErrorMessage(`cQlite query failed: ${err.message}`);
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
  const panel = vscode.window.createWebviewPanel('cQliteResults', `cQlite: ${title}`, vscode.ViewColumn.Beside, {});
  panel.webview.html = page('Results', `
    <section class="toolbar"><strong>${esc(resultSummary(result))}</strong></section>
    <pre>${esc(result.sql || '')}</pre>
    ${tableHtml(result)}
  `);
}

function databaseHtml(dbPath, schema, selected, preview, logs = []) {
  const objects = schemaObjects(schema);
  const options = objects.map(obj => `<button type="button" data-name="${escAttr(obj.name)}"><span>${esc(obj.name)}</span><small>${esc(obj.type || 'table')}</small></button>`).join('');
  const empty = objects.length === 0 ? '<p class="empty">No tables or views found.</p>' : '';
  const previewErrorTemplate = JSON.stringify(errorHtml('__ERROR__'));
  const initialSQL = selected ? defaultObjectSQL(selected) : '';
  const initialPreview = jsJSON(preview || null);
  const objectNames = jsJSON(objects.map(obj => obj.name));
  const schemaJSON = jsJSON({ objects });
  const logsJSON = jsJSON(logs);
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
      <nav class="main-tabs">
        <button data-main-tab="structure">Structure</button>
        <button data-main-tab="data" class="active">Data</button>
        <button data-main-tab="query">Query</button>
        <button data-main-tab="logs">Logs</button>
        <button data-main-tab="manage">Design/Manage</button>
      </nav>
      <section id="tab-structure" class="main-tab" hidden>
        <div id="structureView"></div>
      </section>
      <section id="tab-data" class="main-tab">
        ${empty}
        <div class="data-actions">
          <button id="addRow" title="Add staged row">Add Row</button>
          <button id="deleteRows" title="Stage selected rows for delete">Delete Selected</button>
          <button id="saveEdits" title="Preview and save staged data edits">Save</button>
          <button id="revertEdits" title="Revert staged edits">Revert</button>
        </div>
        <section class="resultbar">
          <div id="dataSummary" class="summary"></div>
          <div class="actions export-actions data-export-actions">
            <button id="dataExportCsv" title="Export rows as CSV" aria-label="Export rows as CSV">CSV</button>
            <button id="dataExportJson" title="Export rows as JSON" aria-label="Export rows as JSON">JSON</button>
            <button id="dataExportSql" title="Export rows as SQL INSERT statements" aria-label="Export rows as SQL INSERT statements">SQL</button>
          </div>
        </section>
        <div id="dataResult"></div>
      </section>
      <section id="tab-query" class="main-tab" hidden>
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
      <section id="tab-logs" class="main-tab" hidden>
        <div class="data-actions">
          <input id="logFilter" type="search" placeholder="Search logs" />
          <button id="clearLogs">Clear Logs</button>
          <button id="exportLogs">Export Logs</button>
        </div>
        <div id="logsView"></div>
      </section>
      <section id="tab-manage" class="main-tab manage-tab" hidden>
        <label>SQL Preview<textarea id="manageSql" spellcheck="false" placeholder="CREATE INDEX ...; DROP TABLE ...;"></textarea></label>
        <div class="data-actions">
          <button id="previewCreateIndex">Create Index Template</button>
          <button id="previewDropObject">Drop Selected Object Template</button>
          <button id="applySchema">Apply Schema SQL</button>
        </div>
      </section>
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
      const dataSummary = document.getElementById('dataSummary');
      const dataResult = document.getElementById('dataResult');
      const exportActions = document.querySelector('.export-actions');
      const dataExportActions = document.querySelector('.data-export-actions');
      const structureView = document.getElementById('structureView');
      const logsView = document.getElementById('logsView');
      const logFilter = document.getElementById('logFilter');
      const manageSql = document.getElementById('manageSql');
      const objectNames = ${objectNames};
      const schemaState = ${schemaJSON};
      let logsState = ${logsJSON};
      let currentResult = ${initialPreview};
      let results = [];
      let activeResult = -1;
      let currentObject = object ? object.value : '';
      let pendingEdits = [];
      document.getElementById('refresh').addEventListener('click', () => vscode.postMessage({ type: 'refresh', selected: object ? object.value : '' }));
      document.getElementById('query').addEventListener('click', () => vscode.postMessage({ type: 'query', name: currentObject, sql: sql.value }));
      document.getElementById('open').addEventListener('click', () => object && vscode.postMessage({ type: 'open', name: object.value }));
      document.getElementById('execute').addEventListener('click', runSql);
      document.getElementById('exportCsv').addEventListener('click', () => exportRows('csv'));
      document.getElementById('exportJson').addEventListener('click', () => exportRows('json'));
      document.getElementById('exportSql').addEventListener('click', () => exportRows('sql'));
      document.getElementById('dataExportCsv').addEventListener('click', () => exportRows('csv'));
      document.getElementById('dataExportJson').addEventListener('click', () => exportRows('json'));
      document.getElementById('dataExportSql').addEventListener('click', () => exportRows('sql'));
      document.getElementById('addRow').addEventListener('click', addRow);
      document.getElementById('deleteRows').addEventListener('click', deleteSelectedRows);
      document.getElementById('saveEdits').addEventListener('click', saveEdits);
      document.getElementById('revertEdits').addEventListener('click', () => { pendingEdits = []; renderData(currentResult); });
      document.getElementById('clearLogs').addEventListener('click', () => vscode.postMessage({ type: 'clearLogs' }));
      document.getElementById('exportLogs').addEventListener('click', () => download('query-logs.json', JSON.stringify(logsState, null, 2), 'application/json'));
      document.getElementById('previewCreateIndex').addEventListener('click', () => {
        manageSql.value = 'CREATE INDEX ' + quoteIdent('idx_' + currentObject + '_column') + ' ON ' + quoteIdent(currentObject) + ' (' + quoteIdent('column_name') + ');';
      });
      document.getElementById('previewDropObject').addEventListener('click', () => {
        const obj = currentSchemaObject();
        manageSql.value = 'DROP ' + ((obj && obj.type === 'view') ? 'VIEW' : 'TABLE') + ' ' + quoteIdent(currentObject) + ';';
      });
      document.getElementById('applySchema').addEventListener('click', () => applySchemaSql());
      logFilter.addEventListener('input', renderLogs);
      for (const tab of document.querySelectorAll('[data-main-tab]')) {
        tab.addEventListener('click', () => activateMainTab(tab.dataset.mainTab));
      }
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
        renderStructure();
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
        renderStructure();
        upsertResult(message.preview || null);
      });
      window.addEventListener('message', event => {
        const message = event.data;
        if (message.type === 'logs') {
          logsState = message.logs || [];
          renderLogs();
        }
        if (message.type === 'applied') {
          pendingEdits = [];
          if (message.logs) logsState = message.logs;
          upsertResult(message.preview || null);
          renderLogs();
        }
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
      renderStructure();
      renderLogs();
      renderData(currentResult);
      activateMainTab('data');
      vscode.postMessage({ type: 'logs' });
      function activateMainTab(name) {
        for (const button of document.querySelectorAll('[data-main-tab]')) {
          button.classList.toggle('active', button.dataset.mainTab === name);
        }
        for (const panel of document.querySelectorAll('.main-tab')) {
          panel.hidden = panel.id !== 'tab-' + name;
        }
      }
      function runSql() {
        const statements = selectedStatements();
        if (statements.length === 0) return;
        showLoading(statements.length);
        for (const statement of statements) {
          vscode.postMessage({ type: 'runSql', name: currentObject, sql: statement, source: 'query' });
        }
      }
      function showLoading(count = 1) {
        response.textContent = count === 1 ? 'Running...' : 'Running ' + count + ' queries...';
        result.innerHTML = '<p class="empty">Loading...</p>';
        updateExportButtons(null);
      }
      function selectedStatements() {
        const start = sql.selectionStart;
        const end = sql.selectionEnd;
        const selected = start !== end ? sql.value.slice(start, end).trim() : '';
        if (selected) return [selected];
        return splitStatements(sql.value);
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
        renderData(data);
        updateExportButtons(data);
      }
      function renderData(data) {
        dataSummary.textContent = resultSummary(data) + (pendingEdits.length ? ' · ' + pendingEdits.length + ' staged changes' : '');
        dataResult.innerHTML = renderEditableTable(data);
        updateDataExportButtons(data);
      }
      function renderEditableTable(data) {
        if (!data || !data.columns || data.columns.length === 0) return '';
        const header = '<th></th>' + data.columns.map(col => '<th>' + escapeHtml(col) + '</th>').join('');
        const body = (data.rows || []).map((row, rowIndex) => '<tr><td><input type="checkbox" data-row-select="' + rowIndex + '"></td>' + row.map((value, colIndex) => '<td contenteditable="true" data-row="' + rowIndex + '" data-col="' + colIndex + '">' + escapeHtml(formatValue(value)) + '</td>').join('') + '</tr>').join('');
        setTimeout(bindEditableCells, 0);
        return '<div class="table-wrap"><table><thead><tr>' + header + '</tr></thead><tbody>' + body + '</tbody></table></div>';
      }
      function bindEditableCells() {
        for (const cell of dataResult.querySelectorAll('[contenteditable][data-row]')) {
          cell.addEventListener('input', () => stageCellEdit(Number(cell.dataset.row), Number(cell.dataset.col), cell.textContent));
        }
      }
      function stageCellEdit(rowIndex, colIndex, value) {
        if (!currentResult || !currentResult.columns || !currentResult.rows) return;
        const keyCol = primaryKeyColumn();
        const keyIndex = currentResult.columns.indexOf(keyCol);
        if (keyIndex < 0) return;
        const col = currentResult.columns[colIndex];
        const keyValue = currentResult.rows[rowIndex][keyIndex];
        const sqlText = 'UPDATE ' + quoteIdent(currentObject) + ' SET ' + quoteIdent(col) + ' = ' + sqlLiteral(value === 'NULL' ? null : value) + ' WHERE ' + quoteIdent(keyCol) + ' = ' + sqlLiteral(keyValue) + ';';
        upsertPending('update:' + keyValue + ':' + col, sqlText);
      }
      function addRow() {
        if (!currentResult || !currentResult.columns) return;
        const cols = currentResult.columns;
        const values = cols.map(() => null);
        const sqlText = 'INSERT INTO ' + quoteIdent(currentObject) + ' (' + cols.map(quoteIdent).join(', ') + ') VALUES (' + values.map(sqlLiteral).join(', ') + ');';
        upsertPending('insert:' + Date.now(), sqlText);
        renderData(currentResult);
      }
      function deleteSelectedRows() {
        if (!currentResult || !currentResult.columns || !currentResult.rows) return;
        const keyCol = primaryKeyColumn();
        const keyIndex = currentResult.columns.indexOf(keyCol);
        for (const box of dataResult.querySelectorAll('[data-row-select]:checked')) {
          const row = currentResult.rows[Number(box.dataset.rowSelect)];
          const keyValue = row[keyIndex];
          upsertPending('delete:' + keyValue, 'DELETE FROM ' + quoteIdent(currentObject) + ' WHERE ' + quoteIdent(keyCol) + ' = ' + sqlLiteral(keyValue) + ';');
        }
        renderData(currentResult);
      }
      function saveEdits() {
        if (pendingEdits.length === 0) return;
        const statements = pendingEdits.map(edit => edit.sql);
        if (!confirm('Apply ' + statements.length + ' staged data changes?\\n\\n' + statements.join('\\n'))) return;
        vscode.postMessage({ type: 'applyStatements', command: 'applyDataEdits', source: 'data', statements, previewSql: statements });
      }
      function upsertPending(key, sqlText) {
        const existing = pendingEdits.find(edit => edit.key === key);
        if (existing) existing.sql = sqlText;
        else pendingEdits.push({ key, sql: sqlText });
        renderData(currentResult);
      }
      function primaryKeyColumn() {
        const obj = currentSchemaObject();
        const pk = obj && obj.columns ? obj.columns.find(col => col.primaryKey) : null;
        return pk ? pk.name : (currentResult && currentResult.columns ? currentResult.columns[0] : '');
      }
      function currentSchemaObject() {
        return (schemaState.objects || []).find(obj => obj.name === currentObject);
      }
      function applySchemaSql() {
        const statements = splitStatements(manageSql.value);
        if (statements.length === 0) return;
        if (!confirm('Apply schema SQL?\\n\\n' + statements.join('\\n'))) return;
        vscode.postMessage({ type: 'applyStatements', command: 'applySchemaChange', source: 'schema', statements, previewSql: statements });
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
      function updateDataExportButtons(data) {
        const hasRows = !!(data && data.rows && data.rows.length > 0);
        dataExportActions.hidden = !hasRows;
        dataExportActions.style.display = hasRows ? 'flex' : 'none';
      }
      function renderStructure() {
        const obj = currentSchemaObject();
        structureView.innerHTML = obj ? '<section class="object-card"><h3>' + escapeHtml(obj.type || 'table') + ': ' + escapeHtml(obj.name) + '</h3><pre>' + escapeHtml(obj.sql || '') + '</pre>' + structureTable(obj) + indexList(obj) + fkList(obj) + triggerList(obj) + '</section>' : '<p class="empty">No object selected.</p>';
      }
      function structureTable(obj) {
        const rows = (obj.columns || []).map(col => '<tr><td>' + escapeHtml(col.name) + '</td><td>' + escapeHtml(col.type || '') + '</td><td>' + (col.primaryKey ? 'PK' : '') + '</td><td>' + (col.notNull ? 'NOT NULL' : '') + '</td><td>' + escapeHtml(col.default ?? '') + '</td></tr>').join('');
        return '<div class="table-wrap"><table><thead><tr><th>Name</th><th>Type</th><th>Key</th><th>Null</th><th>Default</th></tr></thead><tbody>' + rows + '</tbody></table></div>';
      }
      function indexList(obj) {
        if (!obj.indexes || obj.indexes.length === 0) return '';
        return '<h4>Indexes</h4><ul>' + obj.indexes.map(idx => '<li>' + escapeHtml(idx.name) + ' (' + escapeHtml((idx.columns || []).join(', ')) + ')' + (idx.unique ? ' unique' : '') + '</li>').join('') + '</ul>';
      }
      function fkList(obj) {
        if (!obj.foreignKeys || obj.foreignKeys.length === 0) return '';
        return '<h4>Foreign Keys</h4><ul>' + obj.foreignKeys.map(fk => '<li>' + escapeHtml(fk.from || '') + ' → ' + escapeHtml(fk.table || '') + '.' + escapeHtml(fk.to || '') + '</li>').join('') + '</ul>';
      }
      function triggerList(obj) {
        if (!obj.triggers || obj.triggers.length === 0) return '';
        return '<h4>Triggers</h4><ul>' + obj.triggers.map(tr => '<li>' + escapeHtml(tr.name) + '</li>').join('') + '</ul>';
      }
      function renderLogs() {
        const needle = logFilter.value.trim().toLowerCase();
        const rows = logsState.filter(log => !needle || (log.sql || '').toLowerCase().includes(needle) || (log.source || '').toLowerCase().includes(needle)).map(log => '<tr><td>' + escapeHtml(log.timestamp || '') + '</td><td>' + escapeHtml(log.source || '') + '</td><td>' + escapeHtml(log.status || '') + '</td><td>' + escapeHtml(String(log.durationMs || 0)) + ' ms</td><td><button data-rerun="' + escapeAttr(log.sql || '') + '">▶</button></td><td><code>' + escapeHtml(log.sql || '') + '</code></td></tr>').join('');
        logsView.innerHTML = '<div class="table-wrap"><table><thead><tr><th>Time</th><th>Source</th><th>Status</th><th>Latency</th><th></th><th>SQL</th></tr></thead><tbody>' + rows + '</tbody></table></div>';
        for (const button of logsView.querySelectorAll('[data-rerun]')) {
          button.addEventListener('click', () => { sql.value = button.dataset.rerun; activateMainTab('query'); runSql(); });
        }
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
      function escapeAttr(value) {
        return escapeHtml(value).replace(/'/g, '&#39;');
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
      .topbar { position: sticky; top: 0; z-index: 3; display: grid; grid-template-columns: minmax(360px, 1fr) auto; gap: 12px; align-items: center; padding: 12px; border-bottom: 1px solid var(--vscode-panel-border); background: var(--vscode-editor-background); }
      .sql-topbar { grid-template-columns: 1fr; }
      .object-select { display: grid; grid-template-columns: auto minmax(220px, 1fr); gap: 8px; align-items: center; }
      .object-select span { color: var(--vscode-descriptionForeground); font-size: 12px; }
      .combo { position: relative; }
      .combo input { width: 100%; box-sizing: border-box; }
      .combo-list { position: absolute; left: 0; right: 0; top: calc(100% + 2px); z-index: 20; max-height: 240px; overflow-y: auto; border: 1px solid var(--vscode-dropdown-border); background: var(--vscode-dropdown-background); box-shadow: 0 8px 20px rgba(0, 0, 0, 0.28); }
      .combo-list button { width: 100%; display: flex; justify-content: space-between; gap: 12px; align-items: center; border: 0; border-radius: 0; color: var(--vscode-dropdown-foreground); background: transparent; text-align: left; }
      .combo-list button[hidden] { display: none; }
      .combo-list button:hover, .combo-list button:focus { background: var(--vscode-list-hoverBackground); outline: none; }
      .combo-list small { color: var(--vscode-descriptionForeground); }
      .actions, .toolbar { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
      .main-tabs { position: sticky; top: 57px; z-index: 2; display: flex; gap: 2px; overflow-x: auto; padding: 0 12px; border-bottom: 1px solid var(--vscode-panel-border); background: var(--vscode-editor-background); }
      .main-tabs button { flex: 0 0 auto; border: 0; border-radius: 0; color: var(--vscode-descriptionForeground); background: transparent; padding: 10px 12px; border-bottom: 2px solid transparent; }
      .main-tabs button.active { color: var(--vscode-foreground); border-bottom-color: var(--vscode-focusBorder); }
      .main-tab { padding: 12px; }
      .data-actions { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; margin-bottom: 10px; }
      .data-actions input { min-width: 220px; }
      .object-card { padding: 12px 0; border-bottom: 1px solid var(--vscode-panel-border); }
      .object-card h3, .object-card h4 { margin: 0 0 8px; }
      .object-card ul { margin: 0 0 10px; padding-left: 18px; }
      .manage-tab label { display: grid; gap: 6px; color: var(--vscode-descriptionForeground); }
      .manage-tab textarea { height: 180px; padding-right: 8px; }
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
      td[contenteditable="true"] { cursor: text; outline-offset: -2px; }
      td[contenteditable="true"]:focus { outline: 1px solid var(--vscode-focusBorder); background: var(--vscode-input-background); }
      pre { margin: 12px; background: var(--vscode-textCodeBlock-background); padding: 10px; overflow: auto; border-radius: 4px; }
      .empty { padding: 12px; color: var(--vscode-descriptionForeground); }
      .error { color: var(--vscode-errorForeground); }
      @media (max-width: 760px) { .topbar, .object-select { grid-template-columns: 1fr; align-items: stretch; } .main-tabs { top: 94px; } .resultbar { align-items: flex-start; flex-direction: column; } }
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
  await vscode.commands.executeCommand('cQlite.openDatabase');
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

function queryLogs(dbPath) {
  if (!extensionContext) {
    return [];
  }
  dbPath = normalizedDbPath(dbPath);
  const all = extensionContext.globalState.get('cQlite.queryLogs', {});
  return Array.isArray(all[dbPath]) ? all[dbPath] : [];
}

async function appendQueryLog(dbPath, result, source) {
  if (!extensionContext) {
    return;
  }
  dbPath = normalizedDbPath(dbPath);
  const all = extensionContext.globalState.get('cQlite.queryLogs', {});
  const entry = {
    id: `${Date.now()}-${Math.random().toString(16).slice(2)}`,
    database: dbPath,
    source,
    sql: result.sql || '',
    timestamp: new Date().toISOString(),
    durationMs: result.latencyMs ?? result.durationMs ?? 0,
    status: result.error ? 'error' : 'ok',
    error: result.error || '',
    rows: result.rows ? result.rows.length : 0,
    rowsAffected: result.rowsAffected || 0
  };
  all[dbPath] = [...queryLogs(dbPath), entry];
  await extensionContext.globalState.update('cQlite.queryLogs', all);
}

async function clearQueryLogs(dbPath) {
  if (!extensionContext) {
    return;
  }
  dbPath = normalizedDbPath(dbPath);
  const all = extensionContext.globalState.get('cQlite.queryLogs', {});
  all[dbPath] = [];
  await extensionContext.globalState.update('cQlite.queryLogs', all);
}

function normalizedDbPath(dbPath) {
  return dbPath ? path.resolve(dbPath) : '';
}

function config() {
  return vscode.workspace.getConfiguration('cQlite');
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
  const defaultArgs = ['run', './cmd/cqlite-vscode-bridge'];
  const configuredCommand = cfg.get('bridgeCommand', 'go');
  const configuredArgs = cfg.get('bridgeArgs', defaultArgs);
  if (configuredCommand === 'go' && sameStringList(configuredArgs, defaultArgs)) {
    const binary = path.join(cwd, 'bin', process.platform === 'win32' ? 'cqlite-vscode-bridge.exe' : 'cqlite-vscode-bridge');
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
