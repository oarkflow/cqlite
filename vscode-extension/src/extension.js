const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const path = require('path');

let extensionContext;
let activeExplorer;
const queryDocumentDatabases = new Map();
const databaseWorkbenchPanels = new Map();

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
      const tables = objects.filter(object => (object.type || 'table') === 'table');
      const views = objects.filter(object => object.type === 'view');
      return [
        tables.length && { kind: 'group', label: `Tables (${tables.length})`, contextValue: 'tableGroup', icon: new vscode.ThemeIcon('table'), objects: tables },
        views.length && { kind: 'group', label: `Views (${views.length})`, contextValue: 'viewGroup', icon: new vscode.ThemeIcon('eye'), objects: views }
      ].filter(Boolean).map(group => ({ ...group, collapsibleState: vscode.TreeItemCollapsibleState.Expanded }));
    }
    if (item.kind === 'group') {
      return item.objects.map(object => ({
        label: object.name,
        description: `${(object.columns || []).length} columns`,
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
    const key = normalizedDbPath(dbPath);
    let markWorkbenchReady;
    const entry = {
      panel: webviewPanel,
      selected: '',
      rendered: false,
      scriptReady: false,
      ready: new Promise(resolve => { markWorkbenchReady = resolve; }),
      markReady: () => markWorkbenchReady(entry)
    };
    databaseWorkbenchPanels.set(key, entry);
    if (webviewPanel.onDidDispose) {
      webviewPanel.onDidDispose(() => {
        if (databaseWorkbenchPanels.get(key) === entry) databaseWorkbenchPanels.delete(key);
        markWorkbenchReady(null);
      });
    }
    await setDatabase(dbPath).catch(err => {
      console.warn(`cQlite: failed to persist database path: ${err && err.message ? err.message : err}`);
    });
    try {
      await renderDatabasePanel(this.explorer, webviewPanel, dbPath);
      entry.selected = schemaObjects(this.explorer.schema)[0]?.name || '';
      entry.rendered = true;
      if (entry.scriptReady) entry.markReady();
    } catch (err) {
      if (databaseWorkbenchPanels.get(key) === entry) databaseWorkbenchPanels.delete(key);
      markWorkbenchReady(null);
      throw err;
    }
  }
}

async function renderDatabasePanel(explorer, webviewPanel, dbPath, preferredObject = '') {
  const schema = await refreshSchema(explorer, false, dbPath);
  const objects = schemaObjects(schema);
  const selected = objects.some(obj => obj.name === preferredObject) ? preferredObject : (objects[0] ? objects[0].name : '');
  const preview = selected ? await loadObjectRows(dbPath, selected).catch(err => ({ error: err.message })) : null;
  webviewPanel.webview.onDidReceiveMessage(async message => handleDatabaseMessage(explorer, webviewPanel, dbPath, message));
  webviewPanel.webview.html = databaseHtml(dbPath, schema, selected, preview, queryLogs(dbPath));
}

async function handleDatabaseMessage(explorer, webviewPanel, dbPath, message) {
  if (message.type === 'webviewReady') {
    const entry = databaseWorkbenchPanels.get(normalizedDbPath(dbPath));
    if (entry && entry.panel === webviewPanel) {
      entry.scriptReady = true;
      if (entry.rendered) entry.markReady();
    }
    return;
  }
  if (message.type === 'refresh') {
    const nextSchema = await refreshSchema(explorer, false, dbPath);
    const nextObjects = schemaObjects(nextSchema);
    const nextSelected = nextObjects.some(obj => obj.name === message.selected) ? message.selected : (nextObjects[0] ? nextObjects[0].name : '');
    const nextPreview = nextSelected ? await loadObjectRows(dbPath, nextSelected).catch(err => ({ error: err.message })) : null;
    webviewPanel.webview.postMessage({ type: 'refreshResult', schema: nextSchema, selected: nextSelected, preview: nextPreview, requestId: message.requestId });
  }
  if (message.type === 'select' && message.name) {
    const workbench = databaseWorkbenchPanels.get(normalizedDbPath(dbPath));
    if (workbench && workbench.panel === webviewPanel) workbench.selected = message.name;
    const nextPreview = await loadObjectRows(dbPath, message.name).catch(err => ({ error: err.message }));
    webviewPanel.webview.postMessage({ type: 'preview', source: 'table', name: message.name, requestId: message.requestId, sql: defaultObjectSQL(message.name), preview: nextPreview });
  }
  if (message.type === 'browse') {
    const result = await browseObjectRows(dbPath, message.name, message.offset || 0, message.where || '', message.sort || '').catch(err => ({ error: err.message }));
    webviewPanel.webview.postMessage({ type: 'browseResult', result, name: message.name, offset: message.offset || 0, requestId: message.requestId });
  }
  if (message.type === 'runSql') {
    const sql = String(message.sql || '').trim();
    if (!sql) {
      webviewPanel.webview.postMessage({ type: 'sqlResult', result: { error: 'Enter a SQL statement to run.' } });
      return;
    }
    const statements = Array.isArray(message.statements) && message.statements.length ? message.statements : [sql];
    const results = [];
    for (const statement of statements) {
      const result = await runSQLForDatabase(dbPath, statement, 'editor').catch(async err => {
        const failed = { error: err.message, sql: statement };
        await appendQueryLog(dbPath, failed, 'editor');
        return failed;
      });
      results.push(result);
    }
    // DML and DDL both have no result columns. Refresh the visible table after
    // either so the editor reflects committed SQL immediately.
    const databaseChanged = results.some(result => !result.error && (!result.columns || result.columns.length === 0));
    let schema;
    if (databaseChanged) schema = await refreshSchema(explorer, false, dbPath);
    const selected = message.name || '';
    const preview = databaseChanged && selected
      ? await browseObjectRows(dbPath, selected, 0, '', '').catch(err => ({ error: err.message }))
      : null;
    webviewPanel.webview.postMessage({ type: 'sqlResult', results, schema, selected, preview, requestId: message.requestId, logs: queryLogs(dbPath) });
    return;
  }
  if (message.type === 'applyStatements') {
    const applied = await applyStatements(dbPath, message.command || 'applyDataEdits', message.statements || [], message.previewSql || [], message.source || 'manage').catch(async err => {
      const failed = { error: err.message, sql: (message.statements || []).join('\n') };
      await appendQueryLog(dbPath, failed, message.source || 'manage');
      return failed;
    });
    if (!applied.error) {
      if (message.source !== 'data') await refreshSchema(explorer, false, dbPath);
      const refreshed = (message.source === 'data' || message.source === 'import') && message.name
        ? await browseObjectRows(dbPath, message.name, message.offset || 0, message.where || '', message.sort || '').catch(err => ({ error: err.message }))
        : applied;
      const schemaObjectsNow = schemaObjects(explorer.schema);
      const selected = schemaObjectsNow.some(obj => obj.name === message.name) ? message.name : (schemaObjectsNow[0] && schemaObjectsNow[0].name) || '';
      const selectedRows = message.source === 'data' || message.source === 'import' ? refreshed : selected ? await loadObjectRows(dbPath, selected).catch(err => ({ error: err.message })) : null;
      webviewPanel.webview.postMessage({ type: 'applied', success: true, source: message.source || 'manage', saveResult: applied, preview: refreshed, selectedRows, selected, logs: queryLogs(dbPath), schema: message.source === 'data' ? null : explorer.schema });
    } else {
      webviewPanel.webview.postMessage({ type: 'applied', success: false, source: message.source || 'manage', preview: applied, logs: queryLogs(dbPath) });
    }
  }
  if (message.type === 'logs') {
    webviewPanel.webview.postMessage({ type: 'logs', logs: queryLogs(dbPath) });
  }
  if (message.type === 'clearLogs') {
    await clearQueryLogs(dbPath);
    webviewPanel.webview.postMessage({ type: 'logs', logs: [] });
  }
  if (message.type === 'query') {
    await openSQLDocument(dbPath, message.sql || defaultQuerySQL());
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
  activeExplorer = explorer;
  const dbEditorProvider = new DatabaseEditorProvider(explorer);
  vscode.window.registerTreeDataProvider('cQliteExplorer', explorer);
  context.subscriptions.push(vscode.workspace.onDidCloseTextDocument(document => {
    queryDocumentDatabases.delete(document.uri.toString());
  }));

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
        filters: { 'cQlite database': ['cq', 'db'], 'All files': ['*'] }
      });
      if (!selected || selected.length === 0) {
        return;
      }
      const dbPath = selected[0].fsPath;
      await setDatabase(dbPath);
      if (path.extname(dbPath).toLowerCase() === '.cq') {
        await vscode.commands.executeCommand('vscode.openWith', selected[0], 'cQlite.databaseViewer');
      } else {
        const schema = await refreshSchema(explorer);
        if (!schema) return;
        const objects = schemaObjects(schema);
        await openDatabaseObjectPanel(explorer, dbPath, objects[0] ? objects[0].name : '');
      }
    }),
    vscode.commands.registerCommand('cQlite.openExampleDatabase', async () => {
      const workspaceExample = path.join(workspaceRoot(), 'examples', 'complex_queries', 'data', 'complex_embedded.cq');
      let examplePath = fs.existsSync(workspaceExample) ? workspaceExample : '';
      if (!examplePath) {
        const bundledExample = path.join(extensionContext.extensionPath, 'examples', 'complex_embedded.cq');
        if (!fs.existsSync(bundledExample)) {
          vscode.window.showErrorMessage('The cQlite example database is missing from the extension package and workspace.');
          return;
        }
        const storagePath = extensionContext.globalStorageUri
          ? extensionContext.globalStorageUri.fsPath
          : extensionContext.globalStoragePath;
        if (!storagePath) {
          vscode.window.showErrorMessage('VS Code did not provide storage for the cQlite example database.');
          return;
        }
        examplePath = path.join(storagePath, 'examples', 'complex_embedded.cq');
        try {
          await fs.promises.access(examplePath);
        } catch (_) {
          await fs.promises.mkdir(path.dirname(examplePath), { recursive: true });
          await fs.promises.copyFile(bundledExample, examplePath);
        }
      }
      if (!fs.existsSync(examplePath)) {
        vscode.window.showErrorMessage('The cQlite example database is missing from the extension package and workspace.');
        return;
      }
      await vscode.commands.executeCommand('vscode.openWith', vscode.Uri.file(examplePath), 'cQlite.databaseViewer');
    }),
    vscode.commands.registerCommand('cQlite.refresh', async () => refreshSchema(explorer)),
    vscode.commands.registerCommand('cQlite.newQuery', newQuery),
    vscode.commands.registerCommand('cQlite.runQuery', runActiveQuery),
    vscode.commands.registerCommand('cQlite.showTable', async item => {
      const name = item && item.object ? item.object.name : await pickObject(explorer);
      if (name) {
        const db = await ensureDatabase();
        if (db) await openDatabaseObjectPanel(explorer, db, name);
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
  await openWorkbenchForDatabase(db);
  await openSQLDocument(db, defaultQuerySQL());
}

async function openWorkbenchForDatabase(dbPath) {
  const key = normalizedDbPath(dbPath);
  const current = databaseWorkbenchPanels.get(key);
  if (current) return await waitForWorkbench(key, current);
  if (path.extname(dbPath).toLowerCase() !== '.cq') return null;
  try {
    await vscode.commands.executeCommand('vscode.openWith', vscode.Uri.file(dbPath), 'cQlite.databaseViewer');
  } catch (_) {
    return null;
  }
  for (let attempt = 0; attempt < 100; attempt += 1) {
    const entry = databaseWorkbenchPanels.get(key);
    if (entry) {
      return await waitForWorkbench(key, entry);
    }
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  return null;
}

async function waitForWorkbench(key, entry) {
  return new Promise(resolve => {
    const timeout = setTimeout(() => resolve(null), 10000);
    entry.ready.then(ready => {
      clearTimeout(timeout);
      resolve(ready && databaseWorkbenchPanels.get(key) === entry ? entry : null);
    });
  });
}

async function openSQLDocument(dbPath, initialSQL) {
  const document = await vscode.workspace.openTextDocument({ language: 'sql', content: initialSQL || '' });
  queryDocumentDatabases.set(document.uri.toString(), dbPath);
  await vscode.window.showTextDocument(document, { viewColumn: vscode.ViewColumn.Beside, preview: false });
}

async function runActiveQuery() {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    vscode.window.showInformationMessage('Open a SQL editor first.');
    return;
  }
  const selection = editor.selection;
  const sql = selection && !selection.isEmpty ? editor.document.getText(selection) : editor.document.getText();
  let dbPath = queryDocumentDatabases.get(editor.document.uri.toString()) || '';
  if (!dbPath) {
    dbPath = await ensureDatabase();
    if (!dbPath) return;
    queryDocumentDatabases.set(editor.document.uri.toString(), dbPath);
  }
  const statements = splitSqlStatements(sql);
  if (statements.length === 0) {
    vscode.window.showInformationMessage('No SQL to run.');
    return;
  }
  const workbench = await openWorkbenchForDatabase(dbPath);
  if (workbench) {
    const results = [];
    for (const statement of statements) {
      const result = await runSQLForDatabase(dbPath, statement, 'sql-editor').catch(async err => {
        const failed = { error: err.message, sql: statement };
        await appendQueryLog(dbPath, failed, 'sql-editor');
        return failed;
      });
      results.push(result);
    }
    const databaseChanged = results.some(result => !result.error && (!result.columns || result.columns.length === 0));
    const schema = databaseChanged && activeExplorer ? await refreshSchema(activeExplorer, false, dbPath) : undefined;
    const selected = workbench.selected;
    const preview = databaseChanged && selected ? await browseObjectRows(dbPath, selected).catch(err => ({ error: err.message })) : null;
    workbench.panel.webview.postMessage({ type: 'sqlResult', results, sql, schema, selected, preview, logs: queryLogs(dbPath) });
    if (typeof workbench.panel.reveal === 'function') workbench.panel.reveal(vscode.ViewColumn.Active);
    return;
  }
  for (let index = 0; index < statements.length; index += 1) {
    const title = statements.length === 1 ? 'Query Results' : `Query ${index + 1} Results`;
    await runAndShow(statements[index], title, dbPath, 'sql-editor');
  }
}

function splitSqlStatements(text) {
  const statements = [];
  let statement = '';
  let quote = '';
  let lineComment = false;
  let blockComment = false;
  let hasCode = false;
  for (let index = 0; index < text.length; index += 1) {
    const char = text[index];
    const next = text[index + 1];
    statement += char;
    if (lineComment) {
      if (char === '\n') lineComment = false;
      continue;
    }
    if (blockComment) {
      if (char === '*' && next === '/') {
        statement += next;
        index += 1;
        blockComment = false;
      }
      continue;
    }
    if (quote) {
      const closer = quote === '[' ? ']' : quote;
      if (char === closer) {
        if (next === closer) {
          statement += next;
          index += 1;
        } else {
          quote = '';
        }
      }
      continue;
    }
    if (char === '-' && next === '-') {
      statement += next;
      index += 1;
      lineComment = true;
    } else if (char === '/' && next === '*') {
      statement += next;
      index += 1;
      blockComment = true;
    } else if (char === "'" || char === '"' || char === String.fromCharCode(96) || char === '[') {
      quote = char;
      hasCode = true;
    } else if (char === ';') {
      const trimmed = statement.trim();
      if (hasCode && trimmed) statements.push(trimmed);
      statement = '';
      hasCode = false;
    } else if (!/\s/.test(char)) {
      hasCode = true;
    }
  }
  const tail = statement.trim();
  if (hasCode && tail) statements.push(tail);
  return statements;
}

async function loadObjectRows(db, name) {
	return browseObjectRows(db, name, 0, '', '');
}

async function browseObjectRows(db, name, offset = 0, where = '', sort = '') {
  const limit = config().get('resultLimit', 200);
  const payload = JSON.stringify({ table: name, limit, offset, where, sort });
  const result = await bridge(['browseRows', '-payload', payload], { db });
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

async function runAndShow(sql, title, dbOverride = '', source = 'query') {
  const db = dbOverride || await ensureDatabase();
  if (!db) {
    return;
  }
  if (!sql || !sql.trim()) {
    vscode.window.showInformationMessage('No SQL to run.');
    return;
  }
  try {
    const result = await runSQLForDatabase(db, sql, source);
    if ((!result.columns || result.columns.length === 0) && activeExplorer && normalizedDbPath(db) === normalizedDbPath(currentDatabase())) {
      await refreshSchema(activeExplorer, false, db);
    }
    showResultPanel(title, result);
  } catch (err) {
    await appendQueryLog(db, { error: err.message, sql }, source);
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
  const panel = vscode.window.createWebviewPanel('cQliteResults', `cQlite: ${title}`, vscode.ViewColumn.Beside, { enableScripts: true });
  const resultJSON = jsJSON(result || {});
  const exportTools = result && result.columns && result.rows && result.rows.length
    ? `<div class="actions export-actions"><button data-export="csv">${iconSvg('download')}<span>CSV</span></button><button data-export="json">${iconSvg('download')}<span>JSON</span></button><button data-export="sql">${iconSvg('download')}<span>SQL</span></button></div>`
    : '';
  panel.webview.html = page('Results', `
    <section class="toolbar resultbar">
      <strong>${esc(resultSummary(result))}</strong>
      ${exportTools}
    </section>
    <pre>${esc(result.sql || '')}</pre>
    ${tableHtml(result)}
    <script>
      const data = ${resultJSON};
      function download(name, content, type) {
        const url = URL.createObjectURL(new Blob([content], { type }));
        const link = document.createElement('a');
        link.href = url; link.download = name; link.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      }
      function ident(value) { return '"' + String(value).replace(/"/g, '""') + '"'; }
      function literal(value) {
        if (value === null || value === undefined) return 'NULL';
        if (typeof value === 'number') return Number.isFinite(value) ? String(value) : 'NULL';
        if (typeof value === 'boolean') return value ? '1' : '0';
        return "'" + (typeof value === 'object' ? JSON.stringify(value) : String(value)).replace(/'/g, "''") + "'";
      }
      function csv(value) {
        if (value === null || value === undefined) return '';
        const text = typeof value === 'object' ? JSON.stringify(value) : String(value);
        return /[",\\n\\r]/.test(text) ? '"' + text.replace(/"/g, '""') + '"' : text;
      }
      document.querySelectorAll('[data-export]').forEach(button => button.addEventListener('click', () => {
        if (!data.columns || !data.rows) return;
        const kind = button.dataset.export;
        if (kind === 'json') {
          const rows = data.rows.map(row => Object.fromEntries(data.columns.map((column, index) => [column, row[index]])));
          download('query-results.json', JSON.stringify(rows, null, 2), 'application/json');
        } else if (kind === 'csv') {
          download('query-results.csv', [data.columns.map(csv).join(','), ...data.rows.map(row => row.map(csv).join(','))].join('\\n') + '\\n', 'text/csv');
        } else {
          const columns = data.columns.map(ident).join(', ');
          const inserts = data.rows.map(row => 'INSERT INTO "query_results" (' + columns + ') VALUES (' + row.map(literal).join(', ') + ');').join('\\n');
          download('query-results.sql', inserts + '\\n', 'application/sql');
        }
      }));
    </script>
  `);
}

function iconSvg(name) {
  const shapes = {
    database: '<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
    table: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 10h18M9 4v16M15 4v16"/>',
    view: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/>',
    refresh: '<path d="M20 7v5h-5M4 17v-5h5"/><path d="M5.6 9A7 7 0 0 1 18 6l2 2M4 16l2 2a7 7 0 0 0 12.4-3"/>',
    open: '<path d="M14 4h6v6M20 4l-9 9"/><path d="M18 13v6a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h6"/>',
    columns: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16M15 4v16"/>',
    query: '<path d="m8 8-4 4 4 4M16 8l4 4-4 4M14 5l-4 14"/>',
    logs: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2M5 5l2 2M19 5l-2 2"/>',
    settings: '<path d="M4 7h16M4 17h16"/><circle cx="9" cy="7" r="2"/><circle cx="15" cy="17" r="2"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    trash: '<path d="M4 7h16M10 11v6M14 11v6M5 7l1 14h12l1-14M9 7V4h6v3"/>',
    save: '<path d="M5 3h12l4 4v14H3V3h2Z"/><path d="M7 3v6h10V3M7 21v-8h10v8"/>',
    undo: '<path d="M9 14 4 9l5-5"/><path d="M4 9h9a7 7 0 0 1 0 14h-2"/>',
    filter: '<path d="M4 5h16l-6 7v6l-4 2v-8L4 5Z"/>',
    chevronLeft: '<path d="m15 18-6-6 6-6"/>',
    chevronRight: '<path d="m9 18 6-6-6-6"/>',
    close: '<path d="m6 6 12 12M18 6 6 18"/>',
    search: '<circle cx="10.8" cy="10.8" r="6.8"/><path d="m16 16 4.5 4.5"/>',
    download: '<path d="M12 3v12m-5-5 5 5 5-5"/><path d="M4 17v4h16v-4"/>',
    upload: '<path d="M12 16V4m-5 5 5-5 5 5"/><path d="M4 16v4h16v-4"/>',
    play: '<path d="m8 5 12 7-12 7V5Z"/>',
    index: '<path d="M4 6h16M4 12h10M4 18h16"/><circle cx="18" cy="12" r="2"/>'
  };
  return `<svg class="ui-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${shapes[name] || shapes.table}</svg>`;
}

function databaseHtml(dbPath, schema, selected, preview, logs = []) {
  const objects = schemaObjects(schema);
  const tables = objects.filter(obj => (obj.type || 'table') === 'table');
  const views = objects.filter(obj => obj.type === 'view');
  const objectRows = list => list.map(obj => {
    const columnCount = (obj.columns || []).length;
    const objectType = obj.type === 'view' ? 'view' : 'table';
    return `<button type="button" class="object-row${obj.name === selected ? ' active' : ''}" data-name="${escAttr(obj.name)}" title="${escAttr(obj.name + ' · ' + columnCount + ' columns')}" aria-label="${escAttr(objectType + ' ' + obj.name + ', ' + columnCount + ' columns')}"><span class="object-type-icon">${iconSvg(obj.type === 'view' ? 'view' : 'table')}</span><span class="object-row-name">${esc(obj.name)}</span><span class="object-row-count">${columnCount} cols</span></button>`;
  }).join('');
  const initialPreview = jsJSON(preview || null);
  const objectNames = jsJSON(objects.map(obj => obj.name));
  const schemaJSON = jsJSON({ objects });
  const logsJSON = jsJSON(logs);
  return page('Database', `
    <section class="app">
      <header class="topbar">
        <span class="database-mark">${iconSvg('database')}</span>
        <span class="database-label"><span class="database-kind">DATABASE</span><strong title="${escAttr(dbPath)}">${esc(path.basename(dbPath))}</strong><small>${esc(path.dirname(dbPath))}</small></span>
        <span class="connection-state${schema ? '' : ' disconnected'}"><i></i>${schema ? 'Connected' : 'Not connected'}</span>
        <div class="actions topbar-actions">
          <button id="refresh" class="icon-button" title="Refresh database" aria-label="Refresh database">${iconSvg('refresh')}</button>
          <button id="open" class="icon-button" title="Open selected object in a new tab" aria-label="Open selected object in a new tab">${iconSvg('open')}</button>
        </div>
      </header>
      <div class="workbench">
        <aside class="explorer">
          <div class="explorer-heading"><span>OBJECTS</span><span class="explorer-count">${objects.length}</span></div>
          <label class="object-search"><span>${iconSvg('search')}</span><input id="objectSearch" type="search" placeholder="Filter objects" aria-label="Filter objects" /></label>
          <div class="object-group"><div class="object-group-heading"><span>TABLES</span><span id="tableCount">${tables.length}</span></div><div id="tableList" class="object-list">${objectRows(tables) || '<p class="empty-list">No tables</p>'}</div></div>
          <div class="object-group"><div class="object-group-heading"><span>VIEWS</span><span id="viewCount">${views.length}</span></div><div id="viewList" class="object-list">${objectRows(views) || '<p class="empty-list">No views</p>'}</div></div>
          <p id="emptyObjects" class="empty">No tables or views found. Check the database connection, then refresh.</p>
          <input id="object" type="hidden" value="${escAttr(selected)}" />
        </aside>
        <main class="workspace-main">
      <nav class="main-tabs">
        <button data-main-tab="editor" class="active">${iconSvg('table')}<span>Editor</span></button>
        <button data-main-tab="structure">${iconSvg('columns')}<span>Structure</span></button>
        <button data-main-tab="logs">${iconSvg('logs')}<span>History</span></button>
        <button data-main-tab="manage">${iconSvg('settings')}<span>Manage</span></button>
      </nav>
      <section id="tab-structure" class="main-tab" hidden>
        <div id="structureView"></div>
      </section>
      <section id="tab-editor" class="main-tab">
        <section class="inline-sql-editor">
          <div class="inline-sql-heading"><strong>${iconSvg('query')} SQL Editor</strong><span>Run SQL against this database</span></div>
          <div class="sql-editor-surface">
            <pre id="sqlHighlight" aria-hidden="true"><code></code></pre>
            <textarea id="inlineSql" spellcheck="false" wrap="off" aria-label="SQL editor">${esc(defaultObjectSQL(selected))}</textarea>
          </div>
          <div class="inline-sql-footer"><span id="sqlStatus" role="status">Ready</span><button id="runInlineSql" class="primary-action">${iconSvg('play')}<span>Run</span></button></div>
        </section>
        <section id="inlineSqlResults" class="inline-sql-results" hidden></section>
        <div class="data-actions">
          <button id="addRow" title="Add staged row">${iconSvg('plus')}<span>Add row</span></button>
          <button id="deleteRows" title="Stage selected rows for delete">${iconSvg('trash')}<span>Delete</span></button>
          <span id="selectionCount" class="selection-count">0 selected</span>
          <button id="saveEdits" title="Preview and save staged data edits">${iconSvg('save')}<span>Save changes</span></button>
          <button id="revertEdits" title="Revert staged edits">${iconSvg('undo')}<span>Revert</span></button>
          <button id="importCsv" title="Import rows from a CSV file">${iconSvg('upload')}<span>Import CSV</span></button>
          <input id="importCsvFile" type="file" accept=".csv,text/csv" hidden />
          <span class="auto-save-hint" title="Cell edits are saved when you leave the cell">Auto-save on cell exit</span>
          <span class="action-divider"></span>
          <input id="dataFilter" type="search" placeholder="Filter rows, e.g. status = 'open'" aria-label="Filter rows" />
          <button id="applyFilter">${iconSvg('filter')}<span>Filter</span></button>
          <span class="action-spacer"></span>
          <button id="prevPage" class="icon-button" aria-label="Previous page" title="Previous page">${iconSvg('chevronLeft')}</button>
          <span id="pageLabel" class="page-label"></span>
          <button id="nextPage" class="icon-button" aria-label="Next page" title="Next page">${iconSvg('chevronRight')}</button>
        </div>
        <section id="csvImportPreview" class="csv-import-preview" hidden></section>
        <section class="resultbar">
          <div id="dataSummary" class="summary"></div>
          <div class="actions export-actions data-export-actions">
            <button id="dataExportCsv" title="Export rows as CSV" aria-label="Export rows as CSV">${iconSvg('download')}<span>CSV</span></button>
            <button id="dataExportJson" title="Export rows as JSON" aria-label="Export rows as JSON">${iconSvg('download')}<span>JSON</span></button>
            <button id="dataExportSql" title="Export rows as SQL INSERT statements" aria-label="Export rows as SQL INSERT statements">${iconSvg('download')}<span>SQL</span></button>
          </div>
        </section>
        <div id="dataResult"></div>
        <div id="cellValueMenu" class="cell-value-menu" role="dialog" aria-label="Set cell value" hidden>
          <button type="button" data-cell-action="null">Set NULL</button>
          <button type="button" data-cell-action="default">Use column default</button>
          <label>SQL expression<input id="cellSqlExpression" type="text" placeholder="CURRENT_TIMESTAMP" /></label>
          <button type="button" data-cell-action="expression">Apply expression</button>
        </div>
      </section>
      <section id="tab-logs" class="main-tab" hidden>
        <div class="data-actions">
          <input id="logFilter" type="search" placeholder="Search logs" />
          <button id="clearLogs">${iconSvg('trash')}<span>Clear history</span></button>
          <button id="exportLogs">${iconSvg('download')}<span>Export history</span></button>
        </div>
        <div id="logsView"></div>
      </section>
      <section id="tab-manage" class="main-tab manage-tab" hidden>
        <h2>Manage database</h2>
        <p class="manage-intro">Create objects, change table structure, and apply reviewed SQL.</p>
        <details class="manage-panel" open>
        <summary>Create table</summary>
        <section class="new-table-builder">
          <div class="builder-heading"><span>Set columns and constraints, then review the SQL before applying.</span></div>
          <label>Table name<input id="newTableName" type="text" value="new_table" placeholder="table_name" /></label>
          <div id="newTableColumns" class="new-table-columns"></div>
          <div class="actions"><button id="addNewTableColumn">${iconSvg('plus')}<span>Add column</span></button><button id="previewCreateTable" class="primary-action">${iconSvg('table')}<span>Build SQL preview</span></button></div>
        </section>
        </details>
        <details class="manage-panel">
        <summary>Columns</summary>
        <section class="column-builder">
          <label>New column<input id="newColumnName" type="text" placeholder="column_name" /></label>
          <label>Type<input id="newColumnType" type="text" value="TEXT" placeholder="TEXT" /></label>
          <label>Default expression<input id="newColumnDefault" type="text" placeholder="optional, e.g. 0 or CURRENT_TIMESTAMP" /></label>
          <label class="check-label"><input id="newColumnNotNull" type="checkbox" /> Not null</label>
          <button id="previewAddColumn">${iconSvg('plus')}<span>Add column SQL</span></button>
          <label>Rename column<select id="renameColumnSelect"></select></label>
          <label>New name<input id="renameColumnName" type="text" placeholder="new_column_name" /></label>
          <button id="previewRenameColumn">${iconSvg('edit')}<span>Rename SQL</span></button>
          <label>Drop column<select id="dropColumnSelect"></select></label>
          <button id="previewDropColumn" class="danger-action">${iconSvg('trash')}<span>Drop SQL</span></button>
        </section>
        </details>
        <details class="manage-panel">
        <summary>Indexes</summary>
        <section class="index-builder">
          <label>Index name<input id="indexName" type="text" placeholder="idx_table_column" /></label>
          <label>Columns<select id="indexColumns" multiple size="3" aria-label="Index columns"></select></label>
          <label class="check-label"><input id="indexUnique" type="checkbox" /> Unique</label>
          <button id="previewCreateIndex" title="Build a CREATE INDEX statement">${iconSvg('index')}<span>Create index SQL</span></button>
          <label>Existing index<select id="dropIndexSelect"></select></label>
          <button id="previewDropIndex" title="Build a DROP INDEX statement">${iconSvg('trash')}<span>Drop index SQL</span></button>
        </section>
        </details>
        <section class="manage-sql-panel">
        <div class="manage-sql-heading"><strong>SQL preview</strong><span>Review generated SQL or enter your own changes.</span></div>
        <textarea id="manageSql" spellcheck="false" aria-label="SQL preview and schema changes" placeholder="CREATE INDEX ...; DROP TABLE ...;"></textarea>
        <div class="manage-actions">
          <button id="previewCreateView">${iconSvg('view')}<span>New view template</span></button>
          <button id="previewDropObject">${iconSvg('trash')}<span>Drop object template</span></button>
          <span class="action-spacer"></span>
          <button id="applySchema" class="primary-action">${iconSvg('play')}<span>Apply SQL</span></button>
        </div>
        </section>
      </section>
        </main>
      </div>
    </section>
    <script>
      const vscode = acquireVsCodeApi();
      const object = document.getElementById('object');
      const objectSearch = document.getElementById('objectSearch');
      const tableList = document.getElementById('tableList');
      const viewList = document.getElementById('viewList');
      const dataSummary = document.getElementById('dataSummary');
      const dataResult = document.getElementById('dataResult');
      const cellValueMenu = document.getElementById('cellValueMenu');
      let activeValueCell = null;
      window.addEventListener('pointerdown', event => {
        if (cellValueMenu.hidden) return;
        if (event.target && event.target.closest && event.target.closest('#cellValueMenu')) return;
        closeCellValueMenu(false);
      });
      window.addEventListener('keydown', event => {
        if (event.key !== 'Escape' || cellValueMenu.hidden) return;
        event.preventDefault();
        closeCellValueMenu(true);
      });
      const dataExportActions = document.querySelector('.data-export-actions');
      const csvImportPreview = document.getElementById('csvImportPreview');
      const importCsvFile = document.getElementById('importCsvFile');
      const inlineSql = document.getElementById('inlineSql');
      const inlineSqlResults = document.getElementById('inlineSqlResults');
      let inlineSqlResultData = [];
      let activeInlineSqlResult = 0;
      let tableDataIsQueryResult = false;
      let lastSqlPreview = null;
      const structureView = document.getElementById('structureView');
      const logsView = document.getElementById('logsView');
      const logFilter = document.getElementById('logFilter');
      const manageSql = document.getElementById('manageSql');
      const newTableNameInput = document.getElementById('newTableName');
      const newTableColumns = document.getElementById('newTableColumns');
      const indexNameInput = document.getElementById('indexName');
      const indexColumnsSelect = document.getElementById('indexColumns');
      const dropIndexSelect = document.getElementById('dropIndexSelect');
      const renameColumnSelect = document.getElementById('renameColumnSelect');
      const dropColumnSelect = document.getElementById('dropColumnSelect');
      let objectNames = ${objectNames};
      const schemaState = ${schemaJSON};
      let logsState = ${logsJSON};
      let tableData = ${initialPreview};
      let createTableColumns = [
        { name: 'id', type: 'INTEGER', primaryKey: true, notNull: false, unique: false, defaultValue: '' },
        { name: 'name', type: 'TEXT', primaryKey: false, notNull: true, unique: false, defaultValue: '' }
      ];
      let currentObject = object ? object.value : '';
      let pendingEdits = [];
      let draftRow = null;
      let isSavingEdits = false;
      let isLoadingData = false;
      let isImporting = false;
      let pendingCsvImport = null;
      let dataSaveTimer = 0;
      let saveSnapshot = null;
      let queuedObject = '';
      let queuedSql = '';
      let dataOffset = 0;
      let dataWhere = '';
      let dataSort = '';
      let browseRequest = 0;
      let selectionRequest = 0;
      let inlineSqlRequest = 0;
      let inlineSqlRunning = false;
      let pageLimit = ${Number(config().get('resultLimit', 200))};
      document.getElementById('refresh').addEventListener('click', () => {
        if (isImporting) return;
        if ((pendingEdits.length || draftRow) && !confirm('Discard staged changes before refreshing this database?')) return;
        pendingEdits = [];
        draftRow = null;
        isLoadingData = true;
        renderData(tableData);
        dataSummary.textContent = 'Refreshing database…';
        const requestId = ++selectionRequest;
        vscode.postMessage({ type: 'refresh', selected: object ? object.value : '', requestId });
      });
      document.getElementById('runInlineSql').addEventListener('click', runInlineSql);
      inlineSqlResults.addEventListener('click', event => {
        const tab = event.target.closest('[data-result-index]');
        if (tab) {
          activeInlineSqlResult = Number(tab.dataset.resultIndex);
          renderInlineSqlResults();
          applyInlineSqlResultToGrid();
          return;
        }
        const exportButton = event.target.closest('[data-query-export]');
        if (exportButton) exportRows(exportButton.dataset.queryExport, inlineSqlResultData[activeInlineSqlResult]);
      });
      inlineSql.addEventListener('keydown', event => {
        if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') runInlineSql();
        if (event.key === 'Tab') {
          event.preventDefault();
          const start = inlineSql.selectionStart;
          const end = inlineSql.selectionEnd;
          inlineSql.setRangeText('  ', start, end, 'end');
          inlineSql.dispatchEvent(new Event('input', { bubbles: true }));
        }
      });
      inlineSql.addEventListener('input', renderSqlHighlight);
      inlineSql.addEventListener('scroll', syncSqlHighlightScroll);
      document.getElementById('open').addEventListener('click', () => object && vscode.postMessage({ type: 'open', name: object.value }));
      document.getElementById('dataExportCsv').addEventListener('click', () => exportRows('csv', tableData));
      document.getElementById('dataExportJson').addEventListener('click', () => exportRows('json', tableData));
      document.getElementById('dataExportSql').addEventListener('click', () => exportRows('sql', tableData));
      document.getElementById('addRow').addEventListener('click', addRow);
      document.getElementById('deleteRows').addEventListener('click', deleteSelectedRows);
      document.getElementById('saveEdits').addEventListener('click', saveEdits);
      document.getElementById('revertEdits').addEventListener('click', () => { pendingEdits = []; draftRow = null; renderData(tableData); });
      document.getElementById('importCsv').addEventListener('click', () => {
        if (isSavingEdits || pendingEdits.length || draftRow) {
          dataSummary.textContent = 'Save or revert staged table edits before importing a file.';
          return;
        }
        if (!canImportCurrentTable()) {
          dataSummary.textContent = 'CSV import is available when a table is selected.';
          return;
        }
        importCsvFile.click();
      });
      importCsvFile.addEventListener('change', async () => {
        const file = importCsvFile.files && importCsvFile.files[0];
        if (!file) return;
        try {
          pendingCsvImport = buildCsvImport(file.name, await file.text());
          renderCsvImportPreview();
        } catch (error) {
          pendingCsvImport = null;
          csvImportPreview.hidden = false;
          csvImportPreview.innerHTML = '<p class="error">' + escapeHtml(error.message) + '</p><button data-csv-import-action="cancel">Close</button>';
        }
        importCsvFile.value = '';
      });
      csvImportPreview.addEventListener('click', event => {
        const action = event.target.closest('[data-csv-import-action]');
        if (!action) return;
        if (action.dataset.csvImportAction === 'cancel') {
          pendingCsvImport = null;
          csvImportPreview.hidden = true;
          csvImportPreview.innerHTML = '';
        } else if (action.dataset.csvImportAction === 'apply') {
          applyCsvImport();
        }
      });
      document.getElementById('applyFilter').addEventListener('click', applyDataFilter);
      document.getElementById('dataFilter').addEventListener('keydown', event => { if (event.key === 'Enter') applyDataFilter(); });
      document.getElementById('prevPage').addEventListener('click', () => browsePage(Math.max(0, dataOffset - pageLimit)));
      document.getElementById('nextPage').addEventListener('click', () => {
        if (tableData && tableData.rows && tableData.rows.length === pageLimit) browsePage(dataOffset + pageLimit);
      });
      document.getElementById('clearLogs').addEventListener('click', () => vscode.postMessage({ type: 'clearLogs' }));
      document.getElementById('exportLogs').addEventListener('click', () => download('query-logs.json', JSON.stringify(logsState, null, 2), 'application/json'));
      document.getElementById('previewCreateIndex').addEventListener('click', () => {
        const obj = currentSchemaObject();
        const columns = Array.from(indexColumnsSelect.selectedOptions).map(option => option.value);
        const name = indexNameInput.value.trim();
        if (!obj || obj.type !== 'table' || !name || columns.length === 0) return;
        manageSql.value = 'CREATE ' + (document.getElementById('indexUnique').checked ? 'UNIQUE ' : '') + 'INDEX IF NOT EXISTS ' + quoteIdent(name) + ' ON ' + quoteIdent(currentObject) + ' (' + columns.map(quoteIdent).join(', ') + ');';
      });
      document.getElementById('previewDropIndex').addEventListener('click', () => {
        if (dropIndexSelect.value) manageSql.value = 'DROP INDEX IF EXISTS ' + quoteIdent(dropIndexSelect.value) + ';';
      });
      document.getElementById('previewAddColumn').addEventListener('click', () => {
        const name = document.getElementById('newColumnName').value.trim();
        const type = document.getElementById('newColumnType').value.trim();
        if (!currentSchemaObject() || currentSchemaObject().type !== 'table' || !name || !type) return;
        const notNull = document.getElementById('newColumnNotNull').checked ? ' NOT NULL' : '';
        const rawDefault = document.getElementById('newColumnDefault').value.trim();
        const defaultClause = rawDefault ? ' DEFAULT ' + rawDefault : '';
        manageSql.value = 'ALTER TABLE ' + quoteIdent(currentObject) + ' ADD COLUMN ' + quoteIdent(name) + ' ' + type + notNull + defaultClause + ';';
      });
      document.getElementById('previewRenameColumn').addEventListener('click', () => {
        const oldName = renameColumnSelect.value;
        const newName = document.getElementById('renameColumnName').value.trim();
        if (!currentSchemaObject() || currentSchemaObject().type !== 'table' || !oldName || !newName) return;
        manageSql.value = 'ALTER TABLE ' + quoteIdent(currentObject) + ' RENAME COLUMN ' + quoteIdent(oldName) + ' TO ' + quoteIdent(newName) + ';';
      });
      document.getElementById('previewDropColumn').addEventListener('click', () => {
        if (dropColumnSelect.value) manageSql.value = 'ALTER TABLE ' + quoteIdent(currentObject) + ' DROP COLUMN ' + quoteIdent(dropColumnSelect.value) + ';';
      });
      indexColumnsSelect.addEventListener('change', suggestIndexName);
      document.getElementById('addNewTableColumn').addEventListener('click', () => {
        createTableColumns = readCreateTableColumns();
        createTableColumns.push({ name: '', type: 'TEXT', primaryKey: false, notNull: false, unique: false, defaultValue: '' });
        renderCreateTableColumns();
      });
      newTableColumns.addEventListener('click', event => {
        const remove = event.target.closest('[data-remove-create-column]');
        if (!remove) return;
        createTableColumns = readCreateTableColumns();
        createTableColumns.splice(Number(remove.dataset.removeCreateColumn), 1);
        renderCreateTableColumns();
      });
      document.getElementById('previewCreateTable').addEventListener('click', () => {
        const name = newTableNameInput.value.trim();
        const columns = readCreateTableColumns();
        const names = columns.map(column => column.name.trim());
        if (!name) { newTableNameInput.focus(); return; }
        if (columns.length === 0 || columns.some(column => !column.name.trim() || !column.type.trim())) {
          document.getElementById('sqlStatus').textContent = 'Every table column needs a name and type.';
          return;
        }
        if (names.some((columnName, index) => names.slice(0, index).some(previous => previous.toLowerCase() === columnName.toLowerCase()))) {
          document.getElementById('sqlStatus').textContent = 'Column names must be unique.';
          return;
        }
        if (columns.filter(column => column.primaryKey).length > 1) {
          document.getElementById('sqlStatus').textContent = 'Choose one primary key column here. You can edit the preview for a composite key.';
          return;
        }
        if (columns.some(column => !/^[A-Za-z][A-Za-z0-9_ ]*(?:\\s*\\(\\s*\\d+(?:\\s*,\\s*\\d+)?\\s*\\))?$/.test(column.type.trim()))) {
          document.getElementById('sqlStatus').textContent = 'Use a column type such as INTEGER, TEXT, REAL, or VARCHAR(255).';
          return;
        }
        const definitions = columns.map(column => {
          const constraints = [column.primaryKey ? 'PRIMARY KEY' : '', column.notNull ? 'NOT NULL' : '', column.unique ? 'UNIQUE' : '', column.defaultValue.trim() ? 'DEFAULT ' + column.defaultValue.trim() : ''].filter(Boolean);
          return '  ' + quoteIdent(column.name.trim()) + ' ' + column.type.trim() + (constraints.length ? ' ' + constraints.join(' ') : '');
        });
        manageSql.value = 'CREATE TABLE IF NOT EXISTS ' + quoteIdent(name) + ' (\\n' + definitions.join(',\\n') + '\\n);';
        document.getElementById('sqlStatus').textContent = 'Table SQL preview built. Review it, then choose Apply SQL.';
      });
      document.getElementById('previewCreateView').addEventListener('click', () => {
        const source = currentSchemaObject();
        const from = source ? quoteIdent(source.name) : '"your_table"';
        manageSql.value = 'CREATE VIEW IF NOT EXISTS "new_view" AS SELECT * FROM ' + from + ';';
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
      objectSearch.addEventListener('input', filterObjects);
      document.querySelector('.explorer').addEventListener('click', event => {
        const button = event.target.closest('button[data-name]');
        if (button) selectObject(button.dataset.name);
      });
      function selectObject(name) {
        if (!objectNames.includes(name)) return;
        if (name === currentObject) return;
        if (isImporting) {
          dataSummary.textContent = 'Wait for the CSV import to finish before changing tables.';
          return;
        }
        if (isSavingEdits) {
          queuedObject = name;
          dataSummary.textContent = 'Saving changes before opening ' + name + '…';
          return;
        }
        const hasManualEdits = pendingEdits.some(edit => edit.type !== 'update') || !!draftRow;
        if (!hasManualEdits && pendingEdits.some(edit => edit.type === 'update')) {
          queuedObject = name;
          clearTimeout(dataSaveTimer);
          saveEdits(true);
          return;
        }
        if (pendingEdits.length || draftRow) {
          if (!confirm('Discard staged changes for ' + currentObject + ' and open ' + name + '?')) {
            object.value = currentObject;
            return;
          }
          clearTimeout(dataSaveTimer);
          pendingEdits = [];
          draftRow = null;
          updateSaveButton();
        }
        object.value = name;
        currentObject = name;
        renderObjectList();
        tableDataIsQueryResult = false;
        isLoadingData = true;
        tableData = null;
        dataOffset = 0;
        dataWhere = '';
        dataSort = '';
        document.getElementById('dataFilter').value = '';
        renderStructure();
        renderData(null);
        dataSummary.textContent = 'Loading rows…';
        dataResult.innerHTML = '<p class="empty">Loading…</p>';
        const requestId = ++selectionRequest;
        vscode.postMessage({ type: 'select', name, requestId });
      }
      function filterObjects() {
        const needle = objectSearch.value.trim().toLowerCase();
        for (const button of document.querySelectorAll('.object-row[data-name]')) {
          const visible = !needle || button.dataset.name.toLowerCase().includes(needle);
          button.hidden = !visible;
        }
      }
      function renderObjectList() {
        const tables = schemaState.objects.filter(item => (item.type || 'table') === 'table');
        const views = schemaState.objects.filter(item => item.type === 'view');
        const row = item => {
          const columnCount = (item.columns || []).length;
          const objectType = item.type === 'view' ? 'view' : 'table';
          return '<button type="button" class="object-row' + (item.name === currentObject ? ' active' : '') + '" data-name="' + escapeAttr(item.name) + '" title="' + escapeAttr(item.name + ' · ' + columnCount + ' columns') + '" aria-label="' + escapeAttr(objectType + ' ' + item.name + ', ' + columnCount + ' columns') + '"><span class="object-type-icon">' + (item.type === 'view' ? '${iconSvg('view')}' : '${iconSvg('table')}') + '</span><span class="object-row-name">' + escapeHtml(item.name) + '</span><span class="object-row-count">' + columnCount + ' cols</span></button>';
        };
        tableList.innerHTML = tables.map(row).join('') || '<p class="empty-list">No tables</p>';
        viewList.innerHTML = views.map(row).join('') || '<p class="empty-list">No views</p>';
        document.querySelector('.explorer-count').textContent = schemaState.objects.length;
        document.getElementById('tableCount').textContent = tables.length;
        document.getElementById('viewCount').textContent = views.length;
        document.getElementById('emptyObjects').hidden = schemaState.objects.length > 0;
        filterObjects();
      }
      function setConnectionState(connected) {
        const state = document.querySelector('.connection-state');
        state.classList.toggle('disconnected', !connected);
        state.lastChild.textContent = connected ? 'Connected' : 'Not connected';
      }
      window.addEventListener('message', event => {
        const message = event.data;
        if (message.type === 'refreshResult') {
          if (message.requestId !== selectionRequest) return;
          isLoadingData = false;
          if (message.schema) {
            tableDataIsQueryResult = false;
            setConnectionState(true);
            schemaState.objects = message.schema.objects || [];
            objectNames = schemaState.objects.map(item => item.name);
            currentObject = objectNames.includes(message.selected) ? message.selected : (objectNames[0] || '');
            object.value = currentObject;
            renderObjectList();
            dataOffset = 0;
            dataWhere = '';
            dataSort = '';
            document.getElementById('dataFilter').value = '';
            renderStructure();
            tableData = message.preview || null;
            renderData(tableData);
          } else {
            setConnectionState(false);
            dataSummary.textContent = 'Could not refresh the database schema.';
          }
          return;
        }
        if (message.type === 'browseResult') {
          if (message.name !== currentObject) return;
          if (message.requestId !== browseRequest) return;
          isLoadingData = false;
          dataOffset = message.offset || 0;
          tableDataIsQueryResult = false;
          tableData = message.result || null;
          renderData(tableData);
          return;
        }
        if (message.type === 'sqlResult') {
          if (message.requestId !== undefined && message.requestId !== inlineSqlRequest) return;
          isLoadingData = false;
          inlineSqlRunning = false;
          if (message.sql) { inlineSql.value = message.sql; renderSqlHighlight(); }
          inlineSqlResultData = message.results || (message.result ? [message.result] : []);
          lastSqlPreview = message.preview || null;
          activeInlineSqlResult = Math.max(0, inlineSqlResultData.length - 1);
          const failed = inlineSqlResultData.some(result => result.error);
          document.getElementById('sqlStatus').textContent = failed ? 'One or more statements failed' : inlineSqlResultData.map(editorSummary).join(' · ');
          inlineSqlResults.hidden = false;
          renderInlineSqlResults();
          if (message.schema) {
            schemaState.objects = message.schema.objects || [];
            objectNames = schemaState.objects.map(item => item.name);
            currentObject = objectNames.includes(message.selected) ? message.selected : (objectNames[0] || '');
            object.value = currentObject;
            renderObjectList();
            renderStructure();
            if (message.preview) { tableData = message.preview; dataOffset = 0; renderData(tableData); }
          } else if (message.preview && currentObject) {
            tableData = message.preview;
            dataOffset = 0;
            renderData(tableData);
          }
          applyInlineSqlResultToGrid();
          if (message.logs) { logsState = message.logs; renderLogs(); }
          return;
        }
        if (message.type !== 'preview') return;
        if (message.source === 'table') {
          if (message.requestId !== selectionRequest) return;
          isLoadingData = false;
          tableDataIsQueryResult = false;
          if (message.name) {
            currentObject = message.name;
            object.value = currentObject;
            inlineSql.value = defaultSelect(currentObject);
            renderSqlHighlight();
            inlineSqlResults.hidden = true;
            renderObjectList();
          }
          dataOffset = 0;
          dataWhere = '';
          document.getElementById('dataFilter').value = '';
          tableData = message.preview || null;
          renderStructure();
          renderData(tableData);
        }
      });
      window.addEventListener('message', event => {
        const message = event.data;
        if (message.type === 'logs') {
          logsState = message.logs || [];
          renderLogs();
        }
        if (message.type === 'applied') {
          isSavingEdits = false;
          if (message.source === 'data' || message.source === 'import' || message.source === 'schema') isLoadingData = false;
          if (message.source === 'import') isImporting = false;
          if (message.source === 'data') renderData(tableData);
          if (message.success && saveSnapshot) {
            const savedKeys = new Set(saveSnapshot.editKeys);
            pendingEdits = pendingEdits.filter(edit => !savedKeys.has(edit.key));
            if (saveSnapshot.savedDraft) draftRow = null;
            saveSnapshot = null;
          } else if (!message.success) {
            saveSnapshot = null;
          }
          if (message.logs) logsState = message.logs;
          if (message.schema) {
            schemaState.objects = message.schema.objects || [];
            objectNames = (schemaState.objects || []).map(item => item.name);
            currentObject = objectNames.includes(message.selected) ? message.selected : (objectNames[0] || '');
            object.value = currentObject;
            renderObjectList();
            dataOffset = 0;
            dataWhere = '';
            dataSort = '';
            document.getElementById('dataFilter').value = '';
            renderStructure();
          }
          if (message.selectedRows) {
            tableData = message.selectedRows;
            if (message.source !== 'data') dataOffset = 0;
            renderData(tableData);
          } else if (message.source === 'schema' && !(message.preview && message.preview.error)) {
            tableData = null;
            dataOffset = 0;
            renderData(tableData);
          } else if (message.preview && message.preview.error) {
            dataSummary.textContent = 'Save failed: ' + message.preview.error;
          }
          if (message.source === 'data' && message.success) {
            const affected = Number((message.saveResult && message.saveResult.rowsAffected) || 0);
            dataSummary.textContent = message.preview && message.preview.error
              ? 'Changes saved, but rows could not be reloaded: ' + message.preview.error
              : affected === 0
                ? 'Save completed, but no rows matched the update. ' + editorSummary(tableData)
                : 'Saved ' + affected + ' row' + (affected === 1 ? '' : 's') + '. ' + editorSummary(tableData);
          }
          if (message.source === 'import' && message.success) {
            const imported = Number((message.saveResult && message.saveResult.rowsAffected) || 0);
            dataSummary.textContent = 'Imported ' + imported + ' row' + (imported === 1 ? '' : 's') + '. ' + editorSummary(tableData);
            pendingCsvImport = null;
            csvImportPreview.hidden = true;
            csvImportPreview.innerHTML = '';
          }
          if (message.source === 'data' && !message.success) {
            dataSummary.textContent = 'Save failed: ' + ((message.preview && message.preview.error) || 'The database rejected the changes.');
            updateSaveButton();
            queuedObject = '';
          }
          if (message.source === 'import' && !message.success) {
            dataSummary.textContent = 'Import failed: ' + ((message.preview && message.preview.error) || 'The database rejected the CSV rows.');
          }
          if (message.source === 'schema' && !(message.preview && message.preview.error)) dataSummary.textContent = 'Schema changes applied successfully.';
          renderLogs();
          if (message.source === 'data' && message.success && queuedObject) {
            const nextObject = queuedObject;
            queuedObject = '';
            selectObject(nextObject);
          }
          if (message.source === 'data' && queuedSql) {
            if (!message.success) {
              queuedSql = '';
              document.getElementById('sqlStatus').textContent = 'SQL was not run because the pending data save failed.';
            } else if (pendingEdits.length || draftRow) {
              queuedSql = '';
              document.getElementById('sqlStatus').textContent = 'Save or revert the remaining staged changes before running SQL.';
            } else {
              const sql = queuedSql;
              queuedSql = '';
              executeInlineSql(sql);
            }
          }
        }
      });
      renderStructure();
      renderCreateTableColumns();
      renderObjectList();
      renderLogs();
      renderData(tableData);
      activateMainTab('editor');
      renderSqlHighlight();
      vscode.postMessage({ type: 'webviewReady' });
      vscode.postMessage({ type: 'logs' });
      function activateMainTab(name) {
        for (const button of document.querySelectorAll('[data-main-tab]')) {
          button.classList.toggle('active', button.dataset.mainTab === name);
        }
        for (const panel of document.querySelectorAll('.main-tab')) {
          panel.hidden = panel.id !== 'tab-' + name;
        }
      }
      function browsePage(offset) {
        if (tableDataIsQueryResult || inlineSqlRunning || isImporting) return;
        dataOffset = offset;
        isLoadingData = true;
        renderData(tableData);
        dataSummary.textContent = 'Loading rows…';
        browseRequest += 1;
        dataSummary.textContent = 'Loading rows…';
        vscode.postMessage({ type: 'browse', name: currentObject, offset, where: dataWhere, sort: dataSort, requestId: browseRequest });
      }
      function runInlineSql() {
        const sql = inlineSql.value.trim();
        if (!sql) return;
        if (isImporting) {
          document.getElementById('sqlStatus').textContent = 'Wait for the CSV import to finish before running SQL.';
          return;
        }
        const hasManualChanges = !!draftRow || pendingEdits.some(edit => edit.type !== 'update');
        if (hasManualChanges) {
          document.getElementById('sqlStatus').textContent = 'Save or revert staged inserts and deletes before running SQL.';
          return;
        }
        if (isSavingEdits) {
          queuedSql = sql;
          document.getElementById('sqlStatus').textContent = 'Waiting for data changes to save…';
          return;
        }
        if (pendingEdits.some(edit => edit.type === 'update')) {
          queuedSql = sql;
          clearTimeout(dataSaveTimer);
          saveEdits(true);
          document.getElementById('sqlStatus').textContent = 'Saving data changes before running SQL…';
          return;
        }
        executeInlineSql(sql);
      }
      function executeInlineSql(sql) {
        selectionRequest += 1;
        browseRequest += 1;
        const requestId = ++inlineSqlRequest;
        inlineSqlRunning = true;
        renderData(tableData);
        document.getElementById('sqlStatus').textContent = 'Running…';
        inlineSqlResults.hidden = true;
        vscode.postMessage({ type: 'runSql', name: currentObject, sql, statements: splitStatements(sql), requestId });
      }
      function renderSqlHighlight() {
        const code = document.querySelector('#sqlHighlight code');
        if (!code) return;
        const text = inlineSql.value;
        const keywords = new Set(('ABORT ACTION ADD AFTER ALL ALTER ANALYZE AND AS ASC ATTACH AUTOINCREMENT BEFORE BEGIN BETWEEN BY CASCADE CASE CAST CHECK COLLATE COLUMN COMMIT CONFLICT CONSTRAINT CREATE CROSS CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP DATABASE DEFAULT DEFERRABLE DEFERRED DELETE DESC DETACH DISTINCT DROP EACH ELSE END ESCAPE EXCEPT EXCLUDE EXCLUSIVE EXISTS EXPLAIN FAIL FILTER FIRST FOLLOWING FOR FOREIGN FROM FULL GLOB GROUP GROUPS HAVING IF IGNORE IMMEDIATE IN INDEX INDEXED INITIALLY INNER INSERT INSTEAD INTERSECT INTO IS ISNULL JOIN KEY LAST LEFT LIKE LIMIT MATCH MATERIALIZED NATURAL NO NOT NOTHING NOTNULL NULL NULLS OF OFFSET ON OR ORDER OTHERS OUTER OVER PARTITION PLAN PRAGMA PRECEDING PRIMARY QUERY RAISE RANGE RECURSIVE REFERENCES REGEXP REINDEX RELEASE RENAME REPLACE RESTRICT RETURNING RIGHT ROLLBACK ROW ROWS SAVEPOINT SELECT SET STRICT TABLE TEMP TEMPORARY THEN TIES TO TRANSACTION TRIGGER UNBOUNDED UNION UNIQUE UPDATE USING VACUUM VALUES VIEW VIRTUAL WHEN WHERE WINDOW WITH WITHOUT').split(' '));
        const functions = new Set(('AVG COUNT GROUP_CONCAT MAX MIN SUM ABS CHANGES COALESCE GLOB IFNULL INSTR LENGTH LIKE LIKELY LOWER LTRIM NULLIF PRINTF QUOTE REPLACE ROUND RTRIM SIGN SOUNDEX SUBSTR TRIM TYPEOF UNICODE UNLIKELY UPPER ZEROBLOB DATE TIME DATETIME JULIANDAY STRFTIME NOW JSON JSON_ARRAY JSON_OBJECT JSON_EXTRACT').split(' '));
        let html = '';
        let index = 0;
        while (index < text.length) {
          const start = index;
          let kind = '';
          const char = text[index];
          const next = text[index + 1];
          if (char === '-' && next === '-') {
            kind = 'comment';
            index += 2;
            while (index < text.length && text[index] !== '\\n') index += 1;
          } else if (char === '/' && next === '*') {
            kind = 'comment';
            index += 2;
            while (index < text.length && !(text[index] === '*' && text[index + 1] === '/')) index += 1;
            index = Math.min(text.length, index + 2);
          } else if (char === "'") {
            kind = 'string';
            index += 1;
            while (index < text.length) {
              if (text[index] === "'" && text[index + 1] === "'") { index += 2; continue; }
              if (text[index++] === "'") break;
            }
          } else if (char === '"' || char === String.fromCharCode(96) || char === '[') {
            kind = 'identifier';
            const closer = char === '[' ? ']' : char;
            index += 1;
            while (index < text.length && text[index] !== closer) index += 1;
            if (index < text.length) index += 1;
          } else if (/[0-9]/.test(char)) {
            kind = 'number';
            index += 1;
            while (index < text.length && /[0-9A-Fa-f_xX.eE+-]/.test(text[index])) index += 1;
          } else if (/[A-Za-z_]/.test(char)) {
            index += 1;
            while (index < text.length && /[A-Za-z0-9_$]/.test(text[index])) index += 1;
            const word = text.slice(start, index).toUpperCase();
            if (keywords.has(word)) kind = 'keyword';
            else if (functions.has(word)) kind = 'function';
          } else {
            index += 1;
          }
          const value = escapeHtml(text.slice(start, index));
          html += kind ? '<span class="sql-token-' + kind + '">' + value + '</span>' : value;
        }
        code.innerHTML = html + (text.endsWith('\\n') ? ' ' : '');
        syncSqlHighlightScroll();
      }
      function syncSqlHighlightScroll() {
        const highlight = document.getElementById('sqlHighlight');
        highlight.scrollTop = inlineSql.scrollTop;
        highlight.scrollLeft = inlineSql.scrollLeft;
      }
      function renderInlineSqlResults() {
        if (!inlineSqlResultData.length) { inlineSqlResults.innerHTML = ''; return; }
        const tabs = inlineSqlResultData.length > 1
          ? '<nav class="query-result-tabs">' + inlineSqlResultData.map((result, index) => '<button data-result-index="' + index + '" class="' + (index === activeInlineSqlResult ? 'active' : '') + '">' + (result.error ? 'Error ' : 'Result ') + (index + 1) + '</button>').join('') + '</nav>'
          : '';
        const result = inlineSqlResultData[activeInlineSqlResult] || {};
        if (result.error) {
          inlineSqlResults.innerHTML = tabs + '<p class="error">' + escapeHtml(result.error) + '</p>';
          return;
        }
        const exportButtons = result.columns && result.columns.length
          ? '<div class="actions export-actions"><button data-query-export="csv">${iconSvg('download')}<span>CSV</span></button><button data-query-export="json">${iconSvg('download')}<span>JSON</span></button><button data-query-export="sql">${iconSvg('download')}<span>SQL</span></button></div>'
          : '';
        inlineSqlResults.innerHTML = tabs + '<div class="resultbar"><div class="query-result-heading">' + escapeHtml(editorSummary(result)) + '</div>' + exportButtons + '</div>';
      }
      function applyInlineSqlResultToGrid() {
        const result = inlineSqlResultData[activeInlineSqlResult];
        if (result && !result.error && result.columns && result.columns.length) {
          tableData = result;
          dataOffset = 0;
          tableDataIsQueryResult = true;
          renderData(tableData);
        } else if (lastSqlPreview) {
          tableData = lastSqlPreview;
          dataOffset = 0;
          tableDataIsQueryResult = false;
          renderData(tableData);
        }
      }
      function applyDataFilter() {
        if (tableDataIsQueryResult || inlineSqlRunning) return;
        dataWhere = document.getElementById('dataFilter').value.trim();
        browsePage(0);
      }
      function renderData(data) {
        dataSummary.textContent = data && data.error ? 'Could not load rows: ' + data.error : editorSummary(data) + (pendingEdits.length ? ' · ' + pendingEdits.length + ' staged changes' : '');
        const objectInfo = currentSchemaObject();
        const isTable = objectInfo && objectInfo.type === 'table';
        const canEdit = !tableDataIsQueryResult && !inlineSqlRunning && !isImporting && !isLoadingData && isTable && ((objectInfo.columns || []).some(col => col.primaryKey) || !!(data && Array.isArray(data.rowids)));
        document.getElementById('addRow').disabled = tableDataIsQueryResult || inlineSqlRunning || isImporting || isLoadingData || !isTable || isSavingEdits;
        document.getElementById('deleteRows').disabled = !canEdit || isSavingEdits || isImporting;
        document.getElementById('importCsv').disabled = !canImportCurrentTable();
        updateSaveButton();
        if (tableDataIsQueryResult) dataSummary.textContent += ' · SQL results are read-only.';
        else if (isTable && !canEdit) dataSummary.textContent += ' · This table has no primary key or available row ID, so editing and deletion are disabled.';
        const first = data && data.rows && data.rows.length ? dataOffset + 1 : 0;
        const last = data && data.rows ? dataOffset + data.rows.length : 0;
        document.getElementById('pageLabel').textContent = first + '–' + last;
        document.getElementById('prevPage').disabled = tableDataIsQueryResult || inlineSqlRunning || dataOffset === 0;
        document.getElementById('nextPage').disabled = tableDataIsQueryResult || inlineSqlRunning || !data || !data.rows || data.rows.length < pageLimit;
        document.getElementById('applyFilter').disabled = tableDataIsQueryResult || inlineSqlRunning;
        document.getElementById('dataFilter').disabled = tableDataIsQueryResult || inlineSqlRunning;
        dataResult.innerHTML = data && data.error ? '<p class="error">' + escapeHtml(data.error) + '</p>' : renderEditableTable(data);
        updateSelectionControls();
        updateDataExportButtons(data);
      }
      function renderEditableTable(data) {
        if (!data || !data.columns || data.columns.length === 0) return '';
        const objectInfo = currentSchemaObject();
        const canEdit = !tableDataIsQueryResult && !inlineSqlRunning && !isImporting && !isLoadingData && objectInfo && objectInfo.type === 'table' && ((objectInfo.columns || []).some(col => col.primaryKey) || !!(data && Array.isArray(data.rowids)));
        const selectionHeader = tableDataIsQueryResult ? '' : '<th><input id="selectPage" type="checkbox" aria-label="Select all rows on this page" title="Select all rows on this page" ' + (!canEdit ? 'disabled' : '') + '></th>';
        const header = selectionHeader + data.columns.map(col => '<th>' + (tableDataIsQueryResult ? escapeHtml(col) : '<button class="sort-button" data-sort-col="' + escapeAttr(col) + '">' + escapeHtml(col) + (dataSort.startsWith(quoteIdent(col) + ' ') ? (dataSort.endsWith('DESC') ? ' ↓' : ' ↑') : '') + '</button>') + '</th>').join('');
        const body = (data.rows || []).map((row, rowIndex) => '<tr class="' + (isRowStagedForDelete(rowIndex) ? 'staged-delete' : '') + '">' + (tableDataIsQueryResult ? '' : '<td><input type="checkbox" data-row-select="' + rowIndex + '" ' + (!canEdit ? 'disabled' : '') + '></td>') + row.map((value, colIndex) => {
          const column = objectInfo && (objectInfo.columns || []).find(item => item.name === data.columns[colIndex]);
          const title = canEdit ? ' title="Right-click or press Shift+F10 for NULL, default, or SQL expression"' : '';
          const nullClass = value === null || value === undefined ? ' class="null-value" aria-label="SQL NULL"' : '';
          return '<td' + nullClass + ' ' + (canEdit ? 'contenteditable="true" data-row="' + rowIndex + '" data-col="' + colIndex + '"' + title : '') + '>' + escapeHtml(formatValue(value)) + '</td>';
        }).join('') + '</tr>').join('') + (draftRow && !tableDataIsQueryResult ? '<tr class="draft-row"><td>＋</td>' + data.columns.map((name, index) => {
          const column = objectInfo.columns.find(col => col.name === name);
          return column && column.primaryKey && /int/i.test(column.type || '')
            ? '<td><span class="auto-value">auto</span></td>'
            : '<td><input class="draft-cell" data-draft-col="' + index + '" placeholder="' + escapeAttr(name) + '" value="' + escapeAttr(draftRow[index] || '') + '"></td>';
        }).join('') + '</tr>' : '');
        setTimeout(bindEditableCells, 0);
        return '<div class="table-wrap"><table><thead><tr>' + header + '</tr></thead><tbody>' + body + '</tbody></table></div>';
      }
      function bindEditableCells() {
        const selectPage = dataResult.querySelector('#selectPage');
        if (selectPage) selectPage.addEventListener('change', () => {
          for (const checkbox of dataResult.querySelectorAll('[data-row-select]')) checkbox.checked = selectPage.checked;
          updateSelectionControls();
        });
        for (const checkbox of dataResult.querySelectorAll('[data-row-select]')) checkbox.addEventListener('change', updateSelectionControls);
        for (const cell of dataResult.querySelectorAll('[contenteditable][data-row]')) {
          cell.addEventListener('contextmenu', event => { event.preventDefault(); openCellValueMenu(cell, event.clientX, event.clientY); });
          cell.addEventListener('keydown', event => { if (event.shiftKey && event.key === 'F10') { event.preventDefault(); const rect = cell.getBoundingClientRect(); openCellValueMenu(cell, rect.left, rect.bottom); } });
          cell.addEventListener('paste', event => {
            const text = event.clipboardData && event.clipboardData.getData('text/plain');
            if (typeof text !== 'string' || !/[\\t\\r\\n]/.test(text)) return;
            const matrix = parseClipboardGrid(text);
            const startRow = Number(cell.dataset.row);
            const startCol = Number(cell.dataset.col);
            if (startRow + matrix.length > tableData.rows.length || matrix.some(row => startCol + row.length > tableData.columns.length)) {
              event.preventDefault();
              dataSummary.textContent = 'Paste range extends beyond the loaded rows or columns.';
              return;
            }
            event.preventDefault();
            const cells = Array.from(dataResult.querySelectorAll('[contenteditable][data-row]'));
            for (let rowOffset = 0; rowOffset < matrix.length; rowOffset += 1) {
              for (let colOffset = 0; colOffset < matrix[rowOffset].length; colOffset += 1) {
                const rowIndex = startRow + rowOffset;
                const colIndex = startCol + colOffset;
                const value = matrix[rowOffset][colOffset];
                if (isRowStagedForDelete(rowIndex)) continue;
                stageCellEdit(rowIndex, colIndex, value);
                const target = cells.find(item => Number(item.dataset.row) === rowIndex && Number(item.dataset.col) === colIndex);
                if (target) target.textContent = value;
              }
            }
            if (pendingEdits.length) {
              clearTimeout(dataSaveTimer);
              dataSaveTimer = setTimeout(() => saveEdits(true), 250);
            }
          });
          cell.addEventListener('keydown', event => {
            const rowIndex = Number(cell.dataset.row);
            const colIndex = Number(cell.dataset.col);
            if (event.key === 'Enter') {
              event.preventDefault();
              cell.blur();
            } else if (event.key === 'Escape') {
              event.preventDefault();
              cell.textContent = formatValue(tableData.rows[rowIndex][colIndex]);
              cell.classList.toggle('null-value', tableData.rows[rowIndex][colIndex] === null || tableData.rows[rowIndex][colIndex] === undefined);
              cell.blur();
            }
          });
          cell.addEventListener('input', () => {
            const rowIndex = Number(cell.dataset.row);
            const colIndex = Number(cell.dataset.col);
            if (isRowStagedForDelete(rowIndex)) {
              cell.textContent = formatValue(tableData.rows[rowIndex][colIndex]);
              return;
            }
            cell.classList.toggle('null-value', false);
            stageCellEdit(rowIndex, colIndex, cell.textContent);
          });
          cell.addEventListener('blur', () => {
            const rowIndex = Number(cell.dataset.row);
            const colIndex = Number(cell.dataset.col);
            if (isRowStagedForDelete(rowIndex)) return;
            stageCellEdit(rowIndex, colIndex, cell.textContent);
            if (pendingEdits.length) {
              clearTimeout(dataSaveTimer);
              dataSaveTimer = setTimeout(() => saveEdits(true), 250);
            }
          });
        }
        for (const input of dataResult.querySelectorAll('[data-draft-col]')) {
          input.addEventListener('input', () => {
            draftRow[Number(input.dataset.draftCol)] = input.value;
            updateSaveButton();
          });
        }
        for (const button of dataResult.querySelectorAll('[data-sort-col]')) {
          button.addEventListener('click', () => {
            const column = quoteIdent(button.dataset.sortCol);
            dataSort = dataSort === column + ' ASC' ? column + ' DESC' : column + ' ASC';
            browsePage(0);
          });
        }
      }
      function openCellValueMenu(cell, x, y) {
        activeValueCell = cell;
        const rowIndex = Number(cell.dataset.row);
        const colIndex = Number(cell.dataset.col);
        const columnName = tableData.columns[colIndex];
        const obj = currentSchemaObject();
        const column = obj && obj.columns.find(item => item.name === columnName);
        const defaultButton = cellValueMenu.querySelector('[data-cell-action="default"]');
        defaultButton.disabled = !column || column.default === null || column.default === undefined || column.default === '';
        cellValueMenu.hidden = false;
        cellValueMenu.style.left = Math.max(8, Math.min(x, window.innerWidth - 280)) + 'px';
        cellValueMenu.style.top = Math.max(8, Math.min(y, window.innerHeight - 180)) + 'px';
        document.getElementById('cellSqlExpression').value = '';
        cellValueMenu.querySelector('[data-cell-action="null"]').focus();
      }
      function closeCellValueMenu(restoreFocus) {
        cellValueMenu.hidden = true;
        const cell = activeValueCell;
        activeValueCell = null;
        if (restoreFocus && cell && cell.isConnected) cell.focus();
      }
      cellValueMenu.addEventListener('click', event => {
        const action = event.target.closest('[data-cell-action]');
        if (!action || !activeValueCell) return;
        const rowIndex = Number(activeValueCell.dataset.row);
        const colIndex = Number(activeValueCell.dataset.col);
        const column = currentSchemaObject().columns.find(item => item.name === tableData.columns[colIndex]);
        let value;
        if (action.dataset.cellAction === 'null') value = null;
        else if (action.dataset.cellAction === 'default') value = { __cqliteSqlExpression: String(column.default) };
        else {
          const expression = document.getElementById('cellSqlExpression').value.trim();
          if (!expression) return;
          value = { __cqliteSqlExpression: expression };
        }
        stageCellEdit(rowIndex, colIndex, value);
        activeValueCell.classList.toggle('null-value', value === null);
        activeValueCell.textContent = value === null ? 'NULL' : value.__cqliteSqlExpression;
        closeCellValueMenu(false);
        if (pendingEdits.length) { clearTimeout(dataSaveTimer); dataSaveTimer = setTimeout(() => saveEdits(true), 250); }
      });
      function updateSelectionControls() {
        const checkboxes = Array.from(dataResult.querySelectorAll('[data-row-select]'));
        const selectedCount = checkboxes.filter(checkbox => checkbox.checked).length;
        document.getElementById('selectionCount').textContent = selectedCount + ' selected';
        document.getElementById('deleteRows').disabled = isSavingEdits || isImporting || isLoadingData || !currentSchemaObject() || currentSchemaObject().type !== 'table' || selectedCount === 0;
        const selectPage = dataResult.querySelector('#selectPage');
        if (selectPage) {
          selectPage.checked = checkboxes.length > 0 && selectedCount === checkboxes.length;
          selectPage.indeterminate = selectedCount > 0 && selectedCount < checkboxes.length;
        }
      }
      function isRowStagedForDelete(rowIndex) {
        if (!tableData || !tableData.rows || !tableData.rows[rowIndex]) return false;
        const obj = currentSchemaObject();
        const keys = obj && obj.columns ? obj.columns.filter(col => col.primaryKey) : [];
        const keyValues = keys.length
          ? keys.map(col => tableData.rows[rowIndex][tableData.columns.indexOf(col.name)])
          : Array.isArray(tableData.rowids) ? [tableData.rowids[rowIndex]] : [];
        return keyValues.length > 0 && pendingEdits.some(edit => edit.key === 'delete:' + JSON.stringify(keyValues));
      }
      function stageCellEdit(rowIndex, colIndex, value) {
        if (!tableData || !tableData.columns || !tableData.rows) return;
        const obj = currentSchemaObject();
        const keys = obj && obj.columns ? obj.columns.filter(col => col.primaryKey) : [];
        if (keys.length === 0 && !Array.isArray(tableData.rowids)) {
          dataSummary.textContent = 'Editing requires a primary key or a row identifier on this table.';
          return;
        }
        const keyParts = keys.length ? keys.map(col => ({ name: col.name, index: tableData.columns.indexOf(col.name) })) : [{ name: 'rowid', index: -1 }];
        if (keyParts.some(part => part.index < 0 && part.name !== 'rowid') || rowIndex >= tableData.rows.length) return;
        const col = tableData.columns[colIndex];
        const row = tableData.rows[rowIndex];
        const keyValues = keyParts.map(part => part.name === 'rowid' && part.index === -1 ? tableData.rowids[rowIndex] : row[part.index]);
        const original = tableData.rows[rowIndex][colIndex];
        const column = obj.columns.find(item => item.name === col);
        const parsed = value && value.__cqliteSqlExpression ? value : parseEditedValue(value, column, original);
        const rowKey = JSON.stringify(keyValues);
        const deleteKey = 'delete:' + rowKey;
        if (pendingEdits.some(edit => edit.key === deleteKey)) {
          dataSummary.textContent = 'This row is staged for deletion. Revert the delete before editing it.';
          return;
        }
        const key = 'update:' + rowKey;
        let edit = pendingEdits.find(item => item.key === key);
        if (!(parsed && parsed.__cqliteSqlExpression) && ((parsed === null && original === null) || parsed === original || (typeof parsed !== 'object' && String(parsed) === String(original)))) {
          if (edit) {
            delete edit.changes[col];
            if (Object.keys(edit.changes).length === 0) pendingEdits = pendingEdits.filter(item => item !== edit);
          }
        } else {
          if (!edit) {
            edit = { key, type: 'update', keyColumns: keyParts.map(part => part.name), keyValues, changes: {} };
            pendingEdits.push(edit);
          }
          edit.changes[col] = parsed;
        }
        dataSummary.textContent = editorSummary(tableData) + (pendingEdits.length ? ' · ' + pendingEdits.length + ' staged changes' : '');
        updateSaveButton();
      }
      function addRow() {
        if (isImporting || isLoadingData || isSavingEdits) return;
        const obj = currentSchemaObject();
        if (!obj || obj.type !== 'table') return;
        if (!tableData || !tableData.columns) return;
        draftRow = {};
        renderData(tableData);
        const firstEditable = dataResult.querySelector('.draft-cell');
        if (firstEditable) firstEditable.focus();
      }
      function deleteSelectedRows() {
        if (isImporting || isLoadingData || isSavingEdits) return;
        if (!tableData || !tableData.columns || !tableData.rows) return;
        const obj = currentSchemaObject();
        const keys = obj && obj.columns ? obj.columns.filter(col => col.primaryKey) : [];
        if (keys.length === 0 && !Array.isArray(tableData.rowids)) {
          dataSummary.textContent = 'Deleting rows requires a primary key or a row identifier on this table.';
          return;
        }
        const keyParts = keys.length ? keys.map(col => ({ name: col.name, index: tableData.columns.indexOf(col.name) })) : [{ name: 'rowid', index: -1 }];
        if (keyParts.some(part => part.index < 0 && part.name !== 'rowid')) return;
        const selectedRows = Array.from(dataResult.querySelectorAll('[data-row-select]:checked'));
        for (const box of selectedRows) {
          const row = tableData.rows[Number(box.dataset.rowSelect)];
          const keyValues = keyParts.map(part => part.name === 'rowid' && part.index === -1 ? tableData.rowids[Number(box.dataset.rowSelect)] : row[part.index]);
          const rowKey = JSON.stringify(keyValues);
          const key = 'delete:' + rowKey;
          const where = keyParts.map((part, index) => quoteIdent(part.name) + ' = ' + sqlLiteral(keyValues[index])).join(' AND ');
          const sqlText = 'DELETE FROM ' + quoteIdent(currentObject) + ' WHERE ' + where + ';';
          pendingEdits = pendingEdits.filter(edit => edit.key !== 'update:' + rowKey);
          const existing = pendingEdits.find(edit => edit.key === key);
          if (existing) existing.sql = sqlText;
          else pendingEdits.push({ key, sql: sqlText });
        }
        renderData(tableData);
      }
      function saveEdits(automatic = false) {
        if (isSavingEdits || isImporting || isLoadingData) return;
        clearTimeout(dataSaveTimer);
        const editsToSave = automatic ? pendingEdits.filter(edit => edit.type === 'update') : pendingEdits.slice();
        const saveDraft = !automatic && !!draftRow;
        const statements = editsToSave.map(edit => {
          if (edit.type !== 'update') return edit.sql;
          const set = Object.entries(edit.changes).map(([column, value]) => quoteIdent(column) + ' = ' + (value && value.__cqliteSqlExpression ? value.__cqliteSqlExpression : sqlLiteral(value))).join(', ');
          const where = edit.keyColumns.map((column, index) => quoteIdent(column) + ' = ' + sqlLiteral(edit.keyValues[index])).join(' AND ');
          return 'UPDATE ' + quoteIdent(currentObject) + ' SET ' + set + ' WHERE ' + where + ';';
        });
        if (saveDraft) {
          const obj = currentSchemaObject();
          const values = tableData.columns.map((name, index) => ({ col: obj.columns.find(col => col.name === name), value: draftRow[index] }))
            .filter(item => item.col && !(item.col.primaryKey && /int/i.test(item.col.type || '')) && item.value !== undefined && item.value !== '')
            .map(item => ({ col: item.col, value: parseEditedValue(item.value, item.col, null) }));
          if (values.length) statements.push('INSERT INTO ' + quoteIdent(currentObject) + ' (' + values.map(item => quoteIdent(item.col.name)).join(', ') + ') VALUES (' + values.map(item => sqlLiteral(item.value)).join(', ') + ');');
          else statements.push('INSERT INTO ' + quoteIdent(currentObject) + ' DEFAULT VALUES;');
        }
        if (statements.length === 0) return;
        saveSnapshot = { editKeys: editsToSave.map(edit => edit.key), savedDraft: saveDraft };
        isSavingEdits = true;
        updateSaveButton();
        dataSummary.textContent = 'Saving ' + statements.length + ' change' + (statements.length === 1 ? '' : 's') + '…';
        for (const cell of dataResult.querySelectorAll('[contenteditable][data-row]')) cell.contentEditable = 'false';
        for (const input of dataResult.querySelectorAll('[data-draft-col]')) input.disabled = true;
        vscode.postMessage({ type: 'applyStatements', command: 'applyDataEdits', source: 'data', name: currentObject, offset: dataOffset, where: dataWhere, sort: dataSort, statements, previewSql: statements });
      }
      function updateSaveButton() {
        const button = document.getElementById('saveEdits');
        const count = pendingEdits.length + (draftRow ? 1 : 0);
        button.disabled = isSavingEdits || inlineSqlRunning || isLoadingData || count === 0;
        button.classList.toggle('has-pending', count > 0);
        button.querySelector('span').textContent = isSavingEdits ? 'Saving…' : count ? 'Save changes (' + count + ')' : 'Save changes';
      }
      function parseEditedValue(value, column, original) {
        const type = (column && column.type || '').toLowerCase();
        if (value === '') return /bool|int|real|float|double|numeric|decimal/.test(type) || typeof original === 'number' ? null : '';
        if (/bool/.test(type) && /^(true|false|1|0)$/i.test(value)) return /^(true|1)$/i.test(value) ? 1 : 0;
        if (/int|real|float|double|numeric|decimal/.test(type)) {
          const number = Number(value);
          return Number.isFinite(number) ? number : value;
        }
        if (typeof original === 'number') {
          const number = Number(value);
          return Number.isFinite(number) ? number : value;
        }
        return value;
      }
      function parseClipboardGrid(text) {
        const rows = [];
        let row = [];
        let field = '';
        let quoted = false;
        for (let index = 0; index < text.length; index += 1) {
          const char = text[index];
          if (quoted) {
            if (char === '"' && text[index + 1] === '"') { field += '"'; index += 1; }
            else if (char === '"') quoted = false;
            else field += char;
          } else if (char === '"' && field.length === 0) {
            quoted = true;
          } else if (char === '\\t') {
            row.push(field); field = '';
          } else if (char === '\\n' || char === '\\r') {
            row.push(field); field = '';
            rows.push(row); row = [];
            if (char === '\\r' && text[index + 1] === '\\n') index += 1;
          } else {
            field += char;
          }
        }
        row.push(field);
        rows.push(row);
        if (rows.length > 1 && rows[rows.length - 1].length === 1 && rows[rows.length - 1][0] === '' && /[\\r\\n]$/.test(text)) rows.pop();
        return rows;
      }
      function currentSchemaObject() {
        return (schemaState.objects || []).find(obj => obj.name === currentObject);
      }
      function canImportCurrentTable() {
        const obj = currentSchemaObject();
        return !tableDataIsQueryResult && !inlineSqlRunning && !isLoadingData && !isImporting && !isSavingEdits && !pendingEdits.length && !draftRow && obj && obj.type === 'table';
      }
      function parseCsvText(text) {
        const rows = [];
        let row = [];
        let field = '';
        let quoted = false;
        for (let index = 0; index < text.length; index += 1) {
          const char = text[index];
          if (quoted) {
            if (char === '"' && text[index + 1] === '"') { field += '"'; index += 1; }
            else if (char === '"') quoted = false;
            else field += char;
          } else if (char === '"' && field.length === 0) {
            quoted = true;
          } else if (char === ',') {
            row.push(field); field = '';
          } else if (char === '\\n' || char === '\\r') {
            row.push(field); field = '';
            if (!(row.length === 1 && row[0] === '')) rows.push(row);
            row = [];
            if (char === '\\r' && text[index + 1] === '\\n') index += 1;
          } else {
            field += char;
          }
        }
        if (quoted) throw new Error('The CSV contains an unclosed quoted field.');
        if (field.length || row.length) {
          row.push(field);
          if (!(row.length === 1 && row[0] === '')) rows.push(row);
        }
        return rows;
      }
      function buildCsvImport(fileName, csvText) {
        if (csvText.length > 5 * 1024 * 1024) throw new Error('CSV files must be 5 MB or smaller.');
        const obj = currentSchemaObject();
        if (!obj || obj.type !== 'table') throw new Error('Select a table before importing CSV.');
        const parsedRows = parseCsvText(String(csvText).replace(/^\\uFEFF/, ''));
        if (parsedRows.length < 2) throw new Error('The CSV needs a header row and at least one data row.');
        const headers = parsedRows.shift().map(header => header.trim());
        if (headers.some(header => !header)) throw new Error('Every CSV column needs a header.');
        const seen = new Set();
        for (const header of headers) {
          const key = header.toLowerCase();
          if (seen.has(key)) throw new Error('Duplicate CSV column: ' + header);
          seen.add(key);
        }
        const columnsByName = new Map((obj.columns || []).map(column => [column.name.toLowerCase(), column]));
        const columns = headers.map(header => {
          const column = columnsByName.get(header.toLowerCase());
          if (!column) throw new Error('CSV column not found in ' + currentObject + ': ' + header);
          return column;
        });
        const included = new Set(columns.map(column => column.name.toLowerCase()));
        const missingRequired = (obj.columns || []).find(column => column.notNull && column.default === undefined && !column.primaryKey && !included.has(column.name.toLowerCase()));
        if (missingRequired) throw new Error('CSV is missing required column: ' + missingRequired.name);
        const rows = parsedRows.filter(row => !row.every(value => value === ''));
        if (!rows.length) throw new Error('The CSV contains no data rows.');
        if (rows.length > 1000) throw new Error('Import is limited to 1,000 rows per transaction.');
        for (let index = 0; index < rows.length; index += 1) {
          if (rows[index].length !== headers.length) throw new Error('CSV row ' + (index + 2) + ' has ' + rows[index].length + ' values; expected ' + headers.length + '.');
        }
        const statements = rows.map(row => 'INSERT INTO ' + quoteIdent(currentObject) + ' (' + columns.map(column => quoteIdent(column.name)).join(', ') + ') VALUES (' + row.map((value, index) => {
          const column = columns[index];
          if (value === '' && /int|real|float|double|numeric|decimal/i.test(column.type || '')) return 'NULL';
          return sqlLiteral(parseEditedValue(value, column, null));
        }).join(', ') + ');');
        return { fileName, columns: columns.map(column => column.name), rows, statements };
      }
      function renderCsvImportPreview() {
        if (!pendingCsvImport) return;
        const item = pendingCsvImport;
        const sample = item.rows.slice(0, 5);
        const header = item.columns.map(column => '<th>' + escapeHtml(column) + '</th>').join('');
        const body = sample.map(row => '<tr>' + row.map(value => '<td>' + escapeHtml(value) + '</td>').join('') + '</tr>').join('');
        const more = item.rows.length > sample.length ? '<p>Showing ' + sample.length + ' of ' + item.rows.length + ' rows.</p>' : '';
        csvImportPreview.innerHTML = '<div class="csv-import-heading"><strong>Import ' + escapeHtml(item.fileName) + '</strong><span>' + item.rows.length + ' rows · ' + item.columns.length + ' columns into ' + escapeHtml(currentObject) + '</span></div><div class="table-wrap"><table><thead><tr>' + header + '</tr></thead><tbody>' + body + '</tbody></table></div>' + more + '<div class="actions"><button data-csv-import-action="cancel">Cancel</button><button class="primary-action" data-csv-import-action="apply">' + '${iconSvg('upload')}' + '<span>Import ' + item.rows.length + ' rows</span></button></div>';
        csvImportPreview.hidden = false;
      }
      function applyCsvImport() {
        if (!pendingCsvImport || !canImportCurrentTable()) return;
        isImporting = true;
        renderData(tableData);
        dataSummary.textContent = 'Importing ' + pendingCsvImport.rows.length + ' rows…';
        vscode.postMessage({ type: 'applyStatements', command: 'applyDataEdits', source: 'import', name: currentObject, statements: pendingCsvImport.statements, previewSql: [] });
      }
      function readCreateTableColumns() {
        return Array.from(newTableColumns.querySelectorAll('[data-create-table-column]')).map(row => ({
          name: row.querySelector('[data-create-name]').value,
          type: row.querySelector('[data-create-type]').value,
          primaryKey: row.querySelector('[data-create-primary]').checked,
          notNull: row.querySelector('[data-create-not-null]').checked,
          unique: row.querySelector('[data-create-unique]').checked,
          defaultValue: row.querySelector('[data-create-default]').value
        }));
      }
      function renderCreateTableColumns() {
        newTableColumns.innerHTML = createTableColumns.map((column, index) => '<div class="new-table-column" data-create-table-column>' +
          '<label>Column<input data-create-name type="text" value="' + escapeAttr(column.name) + '" placeholder="column_name" /></label>' +
          '<label>Type<input data-create-type type="text" value="' + escapeAttr(column.type) + '" placeholder="TEXT" /></label>' +
          '<label>Default<input data-create-default type="text" value="' + escapeAttr(column.defaultValue) + '" placeholder="optional SQL expression" /></label>' +
          '<label class="check-label"><input data-create-primary type="checkbox" ' + (column.primaryKey ? 'checked' : '') + ' /> Primary key</label>' +
          '<label class="check-label"><input data-create-not-null type="checkbox" ' + (column.notNull ? 'checked' : '') + ' /> Not null</label>' +
          '<label class="check-label"><input data-create-unique type="checkbox" ' + (column.unique ? 'checked' : '') + ' /> Unique</label>' +
          '<button type="button" class="icon-button" data-remove-create-column="' + index + '" title="Remove column" aria-label="Remove column ' + escapeAttr(column.name || (index + 1)) + '">${iconSvg('trash')}</button>' +
          '</div>').join('');
      }
      function applySchemaSql() {
        const statements = splitStatements(manageSql.value);
        if (statements.length === 0) return;
        if (!confirm('Apply schema SQL?\\n\\n' + statements.join('\\n'))) return;
        if (pendingEdits.length || draftRow) {
          if (!confirm('Discard staged table edits before changing the schema?')) return;
          pendingEdits = [];
          draftRow = null;
        }
        vscode.postMessage({ type: 'applyStatements', command: 'applySchemaChange', source: 'schema', name: currentObject, statements, previewSql: statements });
      }
      function splitStatements(text) {
        const statements = [];
        let current = '';
        let quote = '';
        let lineComment = false;
        let blockComment = false;
        let hasCode = false;
        for (let index = 0; index < text.length; index += 1) {
          const char = text[index];
          const next = text[index + 1];
          current += char;
          if (lineComment) {
            if (char.charCodeAt(0) === 10) lineComment = false;
            continue;
          }
          if (blockComment) {
            if (char === '*' && next === '/') {
              current += next;
              index += 1;
              blockComment = false;
            }
            continue;
          }
          if (quote) {
            const closer = quote === '[' ? ']' : quote;
            if (char === closer) {
              if (next === closer) {
                current += next;
                index += 1;
              } else {
                quote = '';
              }
            }
            continue;
          }
          if (char === '-' && next === '-') {
            current += next;
            index += 1;
            lineComment = true;
            continue;
          }
          if (char === '/' && next === '*') {
            current += next;
            index += 1;
            blockComment = true;
            continue;
          }
          if (char === "'" || char === '"' || char === String.fromCharCode(96) || char === '[') {
            quote = char;
            hasCode = true;
            continue;
          }
          if (char === ';') {
            const statement = current.trim();
            if (hasCode && statement) statements.push(statement);
            current = '';
            hasCode = false;
          } else if (!/\s/.test(char)) {
            hasCode = true;
          }
        }
        const tail = current.trim();
        if (hasCode && tail) statements.push(tail);
        return statements;
      }
      function updateDataExportButtons(data) {
        const hasRows = !!(data && data.rows && data.rows.length > 0);
        dataExportActions.hidden = !hasRows;
        dataExportActions.style.display = hasRows ? 'flex' : 'none';
      }
      function renderStructure() {
        const obj = currentSchemaObject();
        renderIndexBuilder(obj);
        structureView.innerHTML = obj ? '<section class="object-card"><h3>' + escapeHtml(obj.type || 'table') + ': ' + escapeHtml(obj.name) + '</h3><pre>' + escapeHtml(obj.sql || '') + '</pre>' + structureTable(obj) + indexList(obj) + fkList(obj) + triggerList(obj) + '</section>' : '<p class="empty">No object selected.</p>';
      }
      function renderIndexBuilder(obj = currentSchemaObject()) {
        const table = obj && obj.type === 'table';
        const columns = table ? (obj.columns || []) : [];
        const columnOptions = columns.map(col => '<option value="' + escapeAttr(col.name) + '">' + escapeHtml(col.name) + '</option>').join('');
        renameColumnSelect.innerHTML = columnOptions;
        dropColumnSelect.innerHTML = columnOptions;
        renameColumnSelect.disabled = !table || columns.length === 0;
        document.getElementById('renameColumnName').disabled = !table || columns.length === 0;
        document.getElementById('previewRenameColumn').disabled = !table || columns.length === 0;
        dropColumnSelect.disabled = !table || columns.length === 0;
        document.getElementById('previewDropColumn').disabled = !table || columns.length === 0;
        document.getElementById('newColumnName').disabled = !table;
        document.getElementById('newColumnType').disabled = !table;
        document.getElementById('newColumnDefault').disabled = !table;
        document.getElementById('newColumnNotNull').disabled = !table;
        document.getElementById('previewAddColumn').disabled = !table;
        const oldColumns = new Set(Array.from(indexColumnsSelect.selectedOptions || []).map(option => option.value));
        const retainedColumns = columns.filter(col => oldColumns.has(col.name));
        const selected = new Set(retainedColumns.length ? retainedColumns.map(col => col.name) : columns.length ? [columns[0].name] : []);
        indexColumnsSelect.innerHTML = columns.map(col => '<option value="' + escapeAttr(col.name) + '" ' + (selected.has(col.name) ? 'selected' : '') + '>' + escapeHtml(col.name) + '</option>').join('');
        indexColumnsSelect.disabled = !table;
        indexNameInput.disabled = !table;
        document.getElementById('indexUnique').disabled = !table;
        document.getElementById('previewCreateIndex').disabled = !table || columns.length === 0;
        const indexes = table ? (obj.indexes || []) : [];
        dropIndexSelect.innerHTML = indexes.map(index => '<option value="' + escapeAttr(index.name) + '">' + escapeHtml(index.name) + '</option>').join('');
        dropIndexSelect.disabled = indexes.length === 0;
        document.getElementById('previewDropIndex').disabled = indexes.length === 0;
        if (!table) indexNameInput.value = '';
        else if (!indexNameInput.value || !currentObject || !indexNameInput.value.startsWith('idx_' + currentObject + '_')) suggestIndexName();
      }
      function suggestIndexName() {
        const selected = Array.from(indexColumnsSelect.selectedOptions).map(option => option.value);
        if (selected.length) indexNameInput.value = 'idx_' + currentObject + '_' + selected.join('_');
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
        const rows = logsState.filter(log => !needle || (log.sql || '').toLowerCase().includes(needle) || (log.source || '').toLowerCase().includes(needle)).map(log => '<tr><td>' + escapeHtml(log.timestamp || '') + '</td><td>' + escapeHtml(log.source || '') + '</td><td>' + escapeHtml(log.status || '') + '</td><td>' + escapeHtml(String(log.durationMs || 0)) + ' ms</td><td><button class="icon-button" data-rerun="' + escapeAttr(log.sql || '') + '" title="Run again" aria-label="Run again">${iconSvg('play')}</button></td><td><code>' + escapeHtml(log.sql || '') + '</code></td></tr>').join('');
        logsView.innerHTML = '<div class="table-wrap"><table><thead><tr><th>Time</th><th>Source</th><th>Status</th><th>Latency</th><th></th><th>SQL</th></tr></thead><tbody>' + rows + '</tbody></table></div>';
        for (const button of logsView.querySelectorAll('[data-rerun]')) {
          button.addEventListener('click', () => {
            const sql = button.dataset.rerun || '';
            if (!sql.trim()) return;
            inlineSql.value = sql;
            renderSqlHighlight();
            activateMainTab('editor');
            runInlineSql();
          });
        }
      }
      function editorSummary(data) {
        if (!data) return 'No rows';
        if (data.columns && data.columns.length > 0) return (data.rows || []).length + ' rows · ' + data.columns.length + ' columns';
        return 'Rows affected: ' + (data.rowsAffected || 0);
      }
      function exportRows(kind, data = tableData) {
        if (!data || !data.columns || !data.rows) return;
        const base = currentObject || 'query-results';
        if (kind === 'json') {
          const rows = data.rows.map(row => Object.fromEntries(data.columns.map((col, index) => [col, row[index]])));
          download(base + '.json', JSON.stringify(rows, null, 2), 'application/json');
        } else if (kind === 'csv') {
          download(base + '.csv', toCsv(data), 'text/csv');
        } else {
          download(base + '.sql', toInsertSql(data, currentObject || 'export_rows'), 'application/sql');
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


function tableHtml(result) {
  if (!result || !result.columns || result.columns.length === 0) {
    return result ? `<section class="summary"><strong>${esc(resultSummary(result))}</strong></section>` : '';
  }
  const header = result.columns.map(col => `<th>${esc(col)}</th>`).join('');
      const body = (result.rows || []).map(row => `<tr>${row.map(value => `<td${value === null || value === undefined ? ' class="null-value" aria-label="SQL NULL"' : ''}>${esc(formatValue(value))}</td>`).join('')}</tr>`).join('');
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
      body { color: var(--vscode-foreground); background: var(--vscode-editor-background); font-family: var(--vscode-font-family); margin: 0; font-size: 13px; }
      [hidden] { display: none !important; }
      .app { min-height: 100vh; display: flex; flex-direction: column; }
      .topbar { position: sticky; top: 0; z-index: 3; display: flex; gap: 12px; align-items: center; min-height: 56px; padding: 0 18px; border-bottom: 1px solid var(--vscode-panel-border); background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .database-mark { display: grid; place-items: center; width: 34px; height: 34px; border-radius: 8px; color: var(--vscode-button-background); background: color-mix(in srgb, var(--vscode-button-background) 14%, transparent); }
      .database-mark .ui-icon { width: 21px; height: 21px; }
      .database-label { display: flex; flex-direction: column; min-width: 0; gap: 2px; color: var(--vscode-foreground); }
      .database-label strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 600; }
      .database-label small { overflow: hidden; max-width: 480px; text-overflow: ellipsis; white-space: nowrap; color: var(--vscode-descriptionForeground); font-size: 11px; }
      .database-kind { color: var(--vscode-descriptionForeground); font-size: 9px; letter-spacing: .1em; }
      .connection-state { display: inline-flex; align-items: center; gap: 6px; margin-left: auto; color: var(--vscode-descriptionForeground); font-size: 11px; }
      .connection-state i { width: 7px; height: 7px; border-radius: 50%; background: var(--vscode-testing-iconPassed, #4caf79); }
      .connection-state.disconnected i { background: var(--vscode-errorForeground, #e05b5b); }
      .topbar-actions { margin-left: 4px; }
      .workbench { display: grid; grid-template-columns: 244px minmax(0, 1fr); flex: 1; min-height: calc(100vh - 57px); }
      .explorer { min-width: 0; padding: 14px 8px; border-right: 1px solid var(--vscode-panel-border); background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .explorer-heading, .object-group-heading { display: flex; justify-content: space-between; align-items: center; padding: 0 9px; color: var(--vscode-descriptionForeground); font-size: 10px; font-weight: 700; letter-spacing: .09em; }
      .explorer-count, .object-group-heading span:last-child { color: var(--vscode-descriptionForeground); font-size: 10px; font-variant-numeric: tabular-nums; letter-spacing: 0; }
      .object-search { display: flex; align-items: center; gap: 7px; margin: 12px 4px 16px; padding: 0 8px; border: 1px solid var(--vscode-input-border, var(--vscode-panel-border)); border-radius: 5px; color: var(--vscode-descriptionForeground); background: var(--vscode-input-background); }
      .object-search .ui-icon { width: 14px; height: 14px; }
      .object-search input { width: 100%; min-width: 0; border: 0; outline: 0; padding: 7px 0; background: transparent; }
      .object-group { margin: 0 0 18px; }
      .object-group-heading { padding: 0 9px 6px; }
      .object-list { display: grid; gap: 2px; }
      .object-row { display: flex; align-items: center; gap: 9px; width: 100%; min-height: 30px; padding: 4px 9px; border: 0; border-radius: 4px; color: var(--vscode-foreground); background: transparent; text-align: left; }
      .object-row:hover { background: var(--vscode-list-hoverBackground); }
      .object-row.active { color: var(--vscode-list-activeSelectionForeground, var(--vscode-foreground)); background: var(--vscode-list-activeSelectionBackground, var(--vscode-list-hoverBackground)); }
      .object-type-icon { display: inline-flex; color: var(--vscode-descriptionForeground); }
      .object-row.active .object-type-icon { color: var(--vscode-focusBorder); }
      .object-row-name { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
      .object-row-count { color: var(--vscode-descriptionForeground); font-size: 10px; font-variant-numeric: tabular-nums; }
      .empty-list { margin: 2px 9px; color: var(--vscode-descriptionForeground); font-size: 11px; }
      .workspace-main { min-width: 0; display: flex; flex-direction: column; }
      .actions, .toolbar { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; }
      .main-tabs { position: sticky; top: 56px; z-index: 2; display: flex; gap: 3px; overflow-x: auto; padding: 0 18px; border-bottom: 1px solid var(--vscode-panel-border); background: var(--vscode-editor-background); }
      .main-tabs button { display: inline-flex; align-items: center; gap: 7px; flex: 0 0 auto; border: 0; border-radius: 0; color: var(--vscode-descriptionForeground); background: transparent; padding: 11px 11px 9px; border-bottom: 2px solid transparent; }
      .main-tabs button.active { color: var(--vscode-foreground); border-bottom-color: var(--vscode-focusBorder); }
      .main-tab { padding: 16px 18px; }
      .inline-sql-editor { margin: 0 0 14px; padding: 12px; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .inline-sql-heading, .inline-sql-footer { display: flex; align-items: center; gap: 8px; }
      .inline-sql-heading { margin-bottom: 8px; }
      .inline-sql-heading strong { display: inline-flex; align-items: center; gap: 7px; font-size: 12px; }
      .inline-sql-heading > span, #sqlStatus { color: var(--vscode-descriptionForeground); font-size: 11px; }
      .inline-sql-editor textarea { height: 112px; min-height: 76px; font-size: var(--vscode-editor-font-size, 13px); }
      .sql-editor-surface { position: relative; }
      #sqlHighlight { position: absolute; z-index: 0; inset: 1px; overflow: hidden; box-sizing: border-box; margin: 0; padding: 6px 9px; border: 0; color: var(--vscode-editor-foreground); background: transparent; white-space: pre; font-family: var(--vscode-editor-font-family); font-size: var(--vscode-editor-font-size, 13px); font-weight: 400; line-height: 1.45; tab-size: 2; pointer-events: none; }
      #sqlHighlight code { font: inherit; }
      #inlineSql { position: relative; z-index: 1; display: block; height: 112px; min-height: 76px; overflow: auto; resize: vertical; box-sizing: border-box; color: transparent; -webkit-text-fill-color: transparent; caret-color: var(--vscode-input-foreground); background: transparent; white-space: pre; font-family: var(--vscode-editor-font-family); font-size: var(--vscode-editor-font-size, 13px); line-height: 1.45; tab-size: 2; }
      #inlineSql::selection { background: var(--vscode-editor-selectionBackground); }
      .sql-token-keyword { color: var(--vscode-symbolIcon-keywordForeground, #569cd6); font-weight: 600; }
      .sql-token-string { color: var(--vscode-debugTokenExpression-string, #ce9178); }
      .sql-token-number { color: var(--vscode-debugTokenExpression-number, #b5cea8); }
      .sql-token-comment { color: var(--vscode-descriptionForeground); font-style: italic; }
      .sql-token-identifier { color: var(--vscode-symbolIcon-variableForeground, var(--vscode-editor-foreground)); }
      .sql-token-function { color: var(--vscode-symbolIcon-functionForeground, #dcdcaa); }
      .inline-sql-footer { justify-content: space-between; margin-top: 8px; }
      .inline-sql-results { margin: 0 0 14px; }
      .query-result-heading { margin-bottom: 8px; color: var(--vscode-descriptionForeground); font-size: 12px; }
      .query-result-tabs { display: flex; gap: 2px; overflow-x: auto; margin-bottom: 8px; border-bottom: 1px solid var(--vscode-panel-border); }
      .query-result-tabs button { flex: 0 0 auto; border: 0; border-bottom: 2px solid transparent; border-radius: 0; color: var(--vscode-descriptionForeground); background: transparent; }
      .query-result-tabs button.active { color: var(--vscode-foreground); border-bottom-color: var(--vscode-focusBorder); }
      .data-actions { display: flex; gap: 7px; align-items: center; flex-wrap: wrap; margin-bottom: 10px; }
      .data-actions input { min-width: 180px; flex: 1; }
      .action-divider { height: 22px; border-left: 1px solid var(--vscode-panel-border); margin: 0 3px; }
      .action-spacer { flex: 1; }
      .selection-count { color: var(--vscode-descriptionForeground); font-size: 11px; white-space: nowrap; }
      .auto-save-hint { color: var(--vscode-descriptionForeground); font-size: 11px; white-space: nowrap; }
      .page-label { color: var(--vscode-descriptionForeground); min-width: 62px; text-align: center; font-variant-numeric: tabular-nums; }
      button:disabled { opacity: .45; cursor: default; }
      .object-card { padding: 12px 0; border-bottom: 1px solid var(--vscode-panel-border); }
      .object-card h3, .object-card h4 { margin: 0 0 8px; }
      .object-card ul { margin: 0 0 10px; padding-left: 18px; }
      .manage-tab label { display: grid; gap: 6px; color: var(--vscode-descriptionForeground); }
      .manage-intro { margin: -8px 0 16px; color: var(--vscode-descriptionForeground); font-size: 12px; }
      .manage-panel { margin: 0 0 10px; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .manage-panel > summary { display: flex; align-items: center; gap: 9px; padding: 11px 13px; cursor: pointer; list-style: none; color: var(--vscode-foreground); font-weight: 600; }
      .manage-panel > summary::-webkit-details-marker { display: none; }
      .manage-panel > summary::before { content: '›'; display: inline-block; width: 12px; color: var(--vscode-descriptionForeground); font-size: 18px; line-height: 12px; transition: transform .12s ease; }
      .manage-panel[open] > summary::before { transform: rotate(90deg); }
      .manage-panel[open] > summary { border-bottom: 1px solid var(--vscode-panel-border); }
      .manage-panel .new-table-builder, .manage-panel .column-builder, .manage-panel .index-builder { margin: 0; border: 0; border-radius: 0; background: transparent; }
      .manage-sql-panel { margin-top: 16px; padding: 13px; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .manage-sql-heading { display: flex; flex-wrap: wrap; align-items: baseline; gap: 9px; margin-bottom: 9px; }
      .manage-sql-heading span { color: var(--vscode-descriptionForeground); font-size: 11px; }
      .manage-sql-panel textarea { display: block; }
      .manage-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin-top: 10px; }
      .manage-actions > button:not(#applySchema) { color: var(--vscode-foreground); background: transparent; border-color: transparent; }
      .manage-actions > button:not(#applySchema):hover { background: var(--vscode-toolbar-hoverBackground, var(--vscode-list-hoverBackground)); }
      .new-table-builder { margin: 12px 0 18px; padding: 12px; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .builder-heading { display: flex; flex-wrap: wrap; align-items: baseline; gap: 8px; margin-bottom: 12px; }
      .builder-heading span { color: var(--vscode-descriptionForeground); font-size: 11px; }
      .new-table-builder > label { max-width: 360px; margin-bottom: 12px; }
      .new-table-columns { display: grid; gap: 8px; margin-bottom: 10px; }
      .new-table-column { display: grid; grid-template-columns: minmax(110px, 1fr) minmax(105px, .8fr) minmax(120px, 1.2fr) repeat(3, auto) 30px; gap: 8px; align-items: end; padding: 8px; border: 1px solid var(--vscode-panel-border); border-radius: 4px; }
      .new-table-column .check-label { display: flex; align-items: center; gap: 5px; min-height: 30px; white-space: nowrap; }
      .new-table-column .check-label input { margin: 0; }
      .new-table-column .icon-button { width: 30px; height: 30px; padding: 5px; }
      .index-builder { display: grid; grid-template-columns: minmax(150px, 1fr) minmax(180px, 1.5fr) auto auto; gap: 10px; align-items: end; margin: 12px 0 18px; padding: 12px; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .column-builder { display: grid; grid-template-columns: repeat(3, minmax(140px, 1fr)); gap: 10px; align-items: end; margin: 12px 0 18px; padding: 12px; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-sideBar-background, var(--vscode-editor-background)); }
      .index-builder select[multiple] { min-height: 74px; }
      .index-builder .check-label { display: inline-flex; align-items: center; gap: 6px; min-height: 30px; }
      .index-builder .check-label input { margin: 0; }
      .column-builder .check-label { display: inline-flex; align-items: center; gap: 6px; min-height: 30px; }
      .column-builder .check-label input { margin: 0; }
      .danger-action { color: var(--vscode-errorForeground); }
      #previewDropIndex { color: var(--vscode-errorForeground); background: transparent; border-color: transparent; }
      .manage-tab textarea { height: 180px; padding-right: 8px; }
      button, input, select, textarea { font: inherit; color: var(--vscode-input-foreground); background: var(--vscode-input-background); border: 1px solid var(--vscode-input-border, var(--vscode-panel-border)); border-radius: 5px; padding: 6px 9px; }
      button { display: inline-flex; align-items: center; justify-content: center; gap: 6px; cursor: pointer; color: var(--vscode-button-foreground); background: var(--vscode-button-background); border-color: var(--vscode-button-border); }
      button:hover { background: var(--vscode-button-hoverBackground, var(--vscode-button-background)); }
      button:focus-visible, input:focus-visible, textarea:focus-visible, select:focus-visible { outline: 1px solid var(--vscode-focusBorder); outline-offset: 1px; }
      .data-actions > button, .export-actions > button, #clearLogs, #exportLogs { color: var(--vscode-foreground); background: transparent; border-color: transparent; }
      .data-actions > button:hover, .export-actions > button:hover, #clearLogs:hover, #exportLogs:hover { background: var(--vscode-toolbar-hoverBackground, var(--vscode-list-hoverBackground)); }
      .primary-action, #saveEdits, #applySchema { color: var(--vscode-button-foreground); background: var(--vscode-button-background); border-color: var(--vscode-button-border); font-weight: 600; }
      .primary-action:hover, #saveEdits:hover, #applySchema:hover { background: var(--vscode-button-hoverBackground, var(--vscode-button-background)); }
      #saveEdits.has-pending { box-shadow: 0 0 0 1px var(--vscode-focusBorder); }
      #deleteRows:not(:disabled), #previewDropObject { color: var(--vscode-errorForeground); }
      .ui-icon { display: inline-block; width: 15px; height: 15px; flex: 0 0 auto; }
      .object-type-icon .ui-icon { width: 14px; height: 14px; }
      .icon-button { width: 32px; height: 32px; display: inline-grid; place-items: center; padding: 0; }
      .icon-button, .secondary-action { color: var(--vscode-foreground); background: transparent; border-color: transparent; }
      .primary-action { padding: 7px 12px; }
      textarea { width: 100%; height: 76px; min-height: 64px; resize: vertical; box-sizing: border-box; font-family: var(--vscode-editor-font-family); line-height: 1.45; padding-right: 82px; }
      .sql-editor textarea { height: 240px; min-height: 150px; }
      .sql-editor { padding: 12px 0 16px; border-bottom: 1px solid var(--vscode-panel-border); }
      .editor-shell { position: relative; }
      .editor-buttons { display: flex; justify-content: flex-end; gap: 7px; margin-bottom: 8px; }
      .preview, .toolbar { padding: 12px 0; }
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
      .table-wrap { overflow: auto; max-height: calc(100vh - 210px); border: 1px solid var(--vscode-panel-border); border-radius: 5px; }
      td.null-value { color: var(--vscode-descriptionForeground); font-style: italic; background: color-mix(in srgb, var(--vscode-editor-background) 88%, var(--vscode-descriptionForeground)); }
      .cell-value-menu { position: fixed; z-index: 1000; display: grid; gap: 6px; min-width: 220px; padding: 10px; background: var(--vscode-menu-background); color: var(--vscode-menu-foreground); border: 1px solid var(--vscode-menu-border, var(--vscode-panel-border)); border-radius: 5px; box-shadow: 0 4px 16px #0006; }
      .cell-value-menu label { display: grid; gap: 4px; font-size: 12px; }
      .cell-value-menu input { width: 100%; box-sizing: border-box; }
      table { border-collapse: separate; border-spacing: 0; width: 100%; min-width: max-content; font-size: 12px; }
      th, td { border-bottom: 1px solid var(--vscode-panel-border); border-right: 1px solid var(--vscode-panel-border); padding: 7px 9px; text-align: left; vertical-align: top; white-space: nowrap; }
      th { position: sticky; top: 0; background: var(--vscode-editorWidget-background); z-index: 1; font-weight: 600; }
      tbody tr:hover { background: var(--vscode-list-hoverBackground); }
      tbody tr.staged-delete td:not(:first-child) { color: var(--vscode-descriptionForeground); text-decoration: line-through; opacity: .7; }
      tbody tr:nth-child(even) { background: color-mix(in srgb, var(--vscode-editor-foreground) 3%, transparent); }
      tbody tr:nth-child(even):hover { background: var(--vscode-list-hoverBackground); }
      .draft-row { background: var(--vscode-editorWidget-background); }
      .draft-row input { min-width: 90px; width: 100%; box-sizing: border-box; }
      .auto-value { color: var(--vscode-descriptionForeground); font-style: italic; }
      .sort-button { padding: 2px 4px; border: 0; border-radius: 2px; background: transparent; color: inherit; font-weight: inherit; }
      .sort-button:hover { background: var(--vscode-toolbar-hoverBackground); }
      tbody tr:last-child td { border-bottom: 0; }
      td { max-width: 420px; overflow: hidden; text-overflow: ellipsis; }
      td[contenteditable="true"] { cursor: text; outline-offset: -2px; }
      td[contenteditable="true"]:focus { outline: 1px solid var(--vscode-focusBorder); background: var(--vscode-input-background); }
      pre { margin: 12px; background: var(--vscode-textCodeBlock-background); padding: 10px; overflow: auto; border-radius: 4px; }
      .empty { padding: 12px; color: var(--vscode-descriptionForeground); }
      .error { color: var(--vscode-errorForeground); }
      @media (max-width: 900px) { .new-table-column { grid-template-columns: repeat(2, minmax(120px, 1fr)); } }
      @media (max-width: 760px) { .workbench { grid-template-columns: 190px minmax(0, 1fr); } .main-tab { padding: 12px; } .main-tabs { padding: 0 10px; } .main-tabs button { padding-left: 8px; padding-right: 8px; } .resultbar { align-items: flex-start; flex-direction: column; } .index-builder, .column-builder { grid-template-columns: 1fr 1fr; } }
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
  let bridgeCommand;
  try {
    bridgeCommand = resolveBridgeCommand(cfg, cwd);
  } catch (err) {
    return Promise.reject(err);
  }
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
  const exe = process.platform === 'win32' ? 'cqlite-vscode-bridge.exe' : 'cqlite-vscode-bridge';
  // 1. Explicit path to a prebuilt bridge binary.
  const explicit = cfg.get('bridgePath', '');
  if (explicit && fs.existsSync(explicit)) {
    return { command: explicit, prefixArgs: [] };
  }
  const defaultArgs = ['run', './cmd/cqlite-vscode-bridge'];
  const configuredCommand = cfg.get('bridgeCommand', 'go');
  const configuredArgs = cfg.get('bridgeArgs', defaultArgs);
  if (configuredCommand === 'go' && sameStringList(configuredArgs, defaultArgs)) {
    // 2. Binary bundled with the installed extension (make install-extension), so
    //    it works from any workspace, not only this repository.
    if (extensionContext && extensionContext.extensionPath) {
      const bundled = path.join(extensionContext.extensionPath, 'bin', exe);
      if (fs.existsSync(bundled)) {
        return { command: bundled, prefixArgs: [] };
      }
    }
    // 3. Binary or source in the open workspace (the cqlite repository itself).
    const local = path.join(cwd, 'bin', exe);
    if (fs.existsSync(local)) {
      return { command: local, prefixArgs: [] };
    }
    if (!fs.existsSync(path.join(cwd, 'cmd', 'cqlite-vscode-bridge'))) {
      throw new Error(
        `cQlite bridge not found. The open workspace (${cwd}) is not the cqlite repository and no bundled bridge is installed. ` +
        'Run "make install-extension" in the cqlite repository, or set cQlite.bridgePath.'
      );
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
