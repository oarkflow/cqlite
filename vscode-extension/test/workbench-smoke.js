const assert = require('assert');
const childProcess = require('child_process');
const fs = require('fs');
const Module = require('module');
const os = require('os');
const path = require('path');
const vm = require('vm');

const root = path.resolve(__dirname, '../..');
const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'cqlite-workbench-'));
const databasePath = path.join(tempRoot, 'example.cq');
fs.copyFileSync(path.join(root, 'examples/complex_queries/data/complex_embedded.cq'), databasePath);

let customProvider;
let runtimeWebview;
const commandHandlers = new Map();
const commandCalls = [];
const extensionOperations = [];
const operationErrors = [];
const settings = {};
const vscode = {
  EventEmitter: class { constructor() { this.event = () => {}; } fire() {} },
  TreeItem: class { constructor(label, state) { this.label = label; this.collapsibleState = state; } },
  TreeItemCollapsibleState: { None: 0, Collapsed: 1, Expanded: 2 },
  ThemeIcon: class { constructor(id) { this.id = id; } },
  ConfigurationTarget: { Workspace: 1, Global: 2 },
  ViewColumn: { Beside: 2 },
  Uri: { file: fsPath => ({ fsPath }) },
  commands: {
    registerCommand: (id, handler) => { commandHandlers.set(id, handler); return { dispose() {} }; },
    executeCommand: async (id, ...args) => { commandCalls.push({ id, args }); }
  },
  window: {
    registerTreeDataProvider: () => ({ dispose() {} }),
    registerCustomEditorProvider: (_id, provider) => { customProvider = provider; return { dispose() {} }; },
    showErrorMessage: message => { throw new Error(message); },
    showInformationMessage: () => {},
    setStatusBarMessage: () => {}
  },
  workspace: {
    workspaceFolders: [{ uri: { fsPath: root } }],
    onDidCloseTextDocument: () => ({ dispose() {} }),
    getConfiguration: () => ({
      get: (key, fallback) => key === 'bridgePath' ? path.join(root, 'bin/cqlite-vscode-bridge') : (settings[key] === undefined ? fallback : settings[key]),
      update: async (key, value) => { settings[key] = value; }
    })
  }
};

const originalLoad = Module._load;
Module._load = function(request, parent, isMain) {
  return request === 'vscode' ? vscode : originalLoad.call(this, request, parent, isMain);
};

function makeWebviewDocument() {
  const elements = new Map();
  const windowListeners = new Map();
  const postedMessages = [];
  const dispatchedMessages = [];
  const timers = [];
  const statusCellListeners = {};
  let createTableRows = [];
  let selectionHtml = '';
  let selectionBoxes = [];
  let draftHtml = '';
  let draftInput = null;
  let sortHtml = '';
  let sortButtons = [];
  let logsHtml = '';
  let rerunButtons = [];
  const statusCell = {
    dataset: { row: '3', col: '2' },
    textContent: 'archived',
    contentEditable: 'true',
    classList: { toggle() {} },
    addEventListener(type, handler) { statusCellListeners[type] = handler; },
    blur() {},
    _listeners: statusCellListeners
  };
  function currentSelectionBoxes() {
    const html = getElement('#dataResult').innerHTML;
    if (html !== selectionHtml) {
      selectionHtml = html;
      selectionBoxes = Array.from(html.matchAll(/data-row-select="(\d+)"/g), match => {
        const listeners = {};
        return { dataset: { rowSelect: match[1] }, checked: false, disabled: false, addEventListener(type, handler) { listeners[type] = handler; }, _listeners: listeners };
      });
    }
    return selectionBoxes;
  }
  function currentDraftInput() {
    const html = getElement('#dataResult').innerHTML;
    if (!html.includes('class="draft-cell"')) return null;
    if (html !== draftHtml) {
      draftHtml = html;
      const listeners = {};
      draftInput = { value: '', disabled: false, dataset: { draftCol: '1' }, addEventListener(type, handler) { listeners[type] = handler; }, focus() {}, _listeners: listeners };
    }
    return draftInput;
  }
  function currentSortButtons() {
    const html = getElement('#dataResult').innerHTML;
    if (html !== sortHtml) {
      sortHtml = html;
      sortButtons = Array.from(html.matchAll(/data-sort-col="([^"]+)"/g), match => {
        const listeners = {};
        return { dataset: { sortCol: match[1] }, addEventListener(type, handler) { listeners[type] = handler; }, _listeners: listeners };
      });
    }
    return sortButtons;
  }
  function currentRerunButtons() {
    const html = getElement('#logsView').innerHTML;
    if (html !== logsHtml) {
      logsHtml = html;
      rerunButtons = Array.from(html.matchAll(/data-rerun="([^"]*)"/g), match => {
        const listeners = {};
        const sql = match[1].replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');
        return { dataset: { rerun: sql }, addEventListener(type, handler) { listeners[type] = handler; }, _listeners: listeners };
      });
    }
    return rerunButtons;
  }
  function getElement(key) {
    if (elements.has(key)) return elements.get(key);
    const listeners = {};
    const element = {
      id: key,
      value: '',
      innerHTML: '',
      textContent: '',
      hidden: key === '#inlineSqlResults',
      disabled: false,
      checked: false,
      indeterminate: false,
      scrollTop: 0,
      scrollLeft: 0,
      selectedOptions: [],
      options: [],
      dataset: {},
      style: {},
      classList: { toggle() {}, add() {}, remove() {} },
      addEventListener(type, handler) { listeners[type] = handler; },
      querySelector(selector) {
        if (key === '#dataResult' && selector === '.draft-cell') return currentDraftInput() || getElement(key + ':draft-cell');
        return getElement(key + ':child');
      },
      querySelectorAll(selector) {
        if (key === '#dataResult' && selector === '[contenteditable][data-row]' && element.innerHTML.includes('contenteditable="true"')) return [statusCell];
        if (key === '#dataResult' && selector === '[data-row-select]') return currentSelectionBoxes();
        if (key === '#dataResult' && selector === '[data-row-select]:checked') return currentSelectionBoxes().filter(box => box.checked);
        if (key === '#dataResult' && selector === '[data-draft-col]') return currentDraftInput() ? [currentDraftInput()] : [];
        if (key === '#dataResult' && selector === '[data-sort-col]') return currentSortButtons();
        if (key === '#logsView' && selector === '[data-rerun]') return currentRerunButtons();
        if (key === '#newTableColumns' && selector === '[data-create-table-column]') return createTableRows;
        return [];
      },
      focus() {},
      blur() {},
      setRangeText() {},
      get selectionStart() { return 0; },
      get selectionEnd() { return 0; },
      _listeners: listeners
    };
    elements.set(key, element);
    return element;
  }
  const document = {
    getElementById: id => getElement('#' + id),
    querySelector: selector => getElement(selector),
    querySelectorAll: () => [],
    createElement: tag => getElement('<' + tag + '>')
  };
  const window = {
    addEventListener(type, handler) {
      if (!windowListeners.has(type)) windowListeners.set(type, []);
      windowListeners.get(type).push(handler);
    }
  };
  const vscodeApi = {
    postMessage(message) {
      postedMessages.push(message);
      if (runtimeWebview && runtimeWebview.onPost) {
        const operation = Promise.resolve(runtimeWebview.onPost(message));
        operation.catch(error => operationErrors.push(error));
        extensionOperations.push(operation);
      }
    }
  };
  return {
    document,
    window,
    vscodeApi,
    postedMessages,
    dispatchedMessages,
    getElement,
    statusCell,
    dataSelectionBoxes: currentSelectionBoxes,
    dataDraftInput: currentDraftInput,
    dataSortButtons: currentSortButtons,
    dataRerunButtons: currentRerunButtons,
    setCreateTableRows(rows) { createTableRows = rows; },
    flushTimers() { while (timers.length) timers.shift()(); },
    setTimeout(handler) { timers.push(handler); return handler; },
    clearTimeout(handler) {
      const index = timers.indexOf(handler);
      if (index >= 0) timers.splice(index, 1);
    },
    dispatchWindow(type, event) {
      for (const handler of windowListeners.get(type) || []) handler(event);
    },
    dispatch(message) {
      dispatchedMessages.push(message);
      for (const handler of windowListeners.get('message') || []) handler({ data: message });
    }
  };
}

async function main() {
  const extension = require(path.join(root, 'vscode-extension/src/extension.js'));
  const globalStateValues = {};
  extension.activate({
    subscriptions: [],
    extensionPath: path.join(root, 'vscode-extension'),
    globalStorageUri: { fsPath: path.join(tempRoot, 'storage') },
    globalState: {
      get: (key, fallback) => globalStateValues[key] === undefined ? fallback : globalStateValues[key],
      update: async (key, value) => { globalStateValues[key] = value; }
    }
  });
  assert(customProvider, 'database custom editor provider was not registered');
  const examplePaths = [
    path.join(root, 'examples/complex_queries/data/complex_embedded.cq'),
    path.join(root, 'vscode-extension/examples/complex_embedded.cq')
  ];
  for (const examplePath of examplePaths) {
    const exampleStats = fs.statSync(examplePath);
    assert(exampleStats.size > 0, '.cq example is missing or empty: ' + examplePath);
    assert(exampleStats.mode & 0o444, '.cq example is not readable: ' + examplePath);
    const result = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
      '-db', examplePath,
      '-sql', 'SELECT id, status, total FROM orders ORDER BY id',
      'execute'
    ], { encoding: 'utf8' }));
    assert.deepStrictEqual(result.rows, [
      [1, 'open', 0],
      [2, 'review', 64.25],
      [3, 'review', 133],
      [4, 'archived', 9.25]
    ], '.cq example does not contain the complete seeded orders data: ' + examplePath);
  }
  await commandHandlers.get('cQlite.openExampleDatabase')();
  const exampleOpen = commandCalls.find(call => call.id === 'vscode.openWith');
  assert(exampleOpen, 'Open Example Database did not open a custom editor');
  assert.strictEqual(exampleOpen.args[0].fsPath, path.join(root, 'examples/complex_queries/data/complex_embedded.cq'), 'example command opened an unexpected .cq file');
  assert.strictEqual(exampleOpen.args[1], 'cQlite.databaseViewer', 'example command did not use the database workbench');
  const panel = {
    webview: {
      options: {},
      html: '',
      onDidReceiveMessage(handler) { this.receive = handler; },
      postMessage(message) { if (runtimeWebview) runtimeWebview.dispatch(message); }
    }
  };
  await customProvider.resolveCustomEditor({ uri: { fsPath: databasePath } }, panel);
  assert(panel.webview.html.includes('<summary>Columns</summary>') && panel.webview.html.includes('<summary>Indexes</summary>'), 'Manage view is missing grouped column and index controls');
  assert(panel.webview.html.includes('class="manage-sql-panel"'), 'Manage view is missing its dedicated SQL preview area');
  assert(panel.webview.html.includes('6 cols</span>') && panel.webview.html.includes('6 columns'), 'object list does not explain its table/view column counts');
  const scripts = Array.from(panel.webview.html.matchAll(/<script>([\s\S]*?)<\/script>/g), match => match[1]);
  assert(scripts.length, 'workbench webview script was not generated');
  const script = scripts.join('\n');
  assert(!script.includes('iconSvg('), 'extension-only icon helper leaked into the webview');
  new vm.Script(script, { filename: 'cqlite-workbench-webview.js' });

  const webview = makeWebviewDocument();
  runtimeWebview = webview;
  runtimeWebview.onPost = message => panel.webview.receive(message);
  const context = {
    document: webview.document,
    window: webview.window,
    acquireVsCodeApi: () => webview.vscodeApi,
    setTimeout: (handler, delay) => webview.setTimeout(handler, delay),
    clearTimeout: handler => webview.clearTimeout(handler),
    confirm: () => true,
    console,
    Blob: class {},
    URL: { createObjectURL: () => '', revokeObjectURL() {} },
    Event: class { constructor(type) { this.type = type; } }
  };
  vm.runInNewContext(script, context, { filename: 'cqlite-workbench-webview.js' });

  webview.getElement('.explorer')._listeners.click({
    target: { closest: () => ({ dataset: { name: 'orders' } }) }
  });
  const selection = webview.postedMessages.find(message => message.type === 'select');
  assert(selection, 'table selection message was not sent');
  await waitFor(() => webview.getElement('#dataSummary').textContent.startsWith('4 rows'), 'orders table load');
  webview.flushTimers();

  webview.statusCell.textContent = 'shipped';
  webview.statusCell._listeners.blur();
  webview.flushTimers();
  const save = webview.postedMessages.find(message => message.type === 'applyStatements' && message.source === 'data');
  assert(save && save.statements[0].includes("SET \"status\" = 'shipped'"), 'cell edit did not queue a database update');
  await Promise.all(extensionOperations);
  webview.statusCell.textContent = '';
  webview.statusCell._listeners.blur();
  webview.flushTimers();
  const emptyTextSave = webview.postedMessages.filter(message => message.type === 'applyStatements' && message.source === 'data').at(-1);
  assert(emptyTextSave && emptyTextSave !== save && emptyTextSave.statements[0].includes('SET "status" = \'\''), 'clearing a text cell did not stage an empty string');
  await Promise.all(extensionOperations);
  const emptyTextResult = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', 'SELECT status FROM orders WHERE id = 4', 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(emptyTextResult.rows, [['']], 'clearing a text cell did not persist an empty string distinct from NULL');
  webview.statusCell.dataset.col = '5';
  webview.statusCell._listeners.contextmenu({ preventDefault() {}, clientX: 20, clientY: 20 });
  assert.strictEqual(webview.getElement('#cellValueMenu').hidden, false, 'cell context menu did not open');
  webview.dispatchWindow('keydown', { key: 'Escape', preventDefault() {} });
  assert.strictEqual(webview.getElement('#cellValueMenu').hidden, true, 'Escape did not close the cell menu');
  webview.statusCell._listeners.contextmenu({ preventDefault() {}, clientX: 20, clientY: 20 });
  webview.getElement('#cellValueMenu')._listeners.click({ target: { closest: () => ({ dataset: { cellAction: 'null' } }) } });
  webview.flushTimers();
  await Promise.all(extensionOperations);
  const nullResult = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', 'SELECT metadata FROM orders WHERE id = 4', 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(nullResult.rows, [[null]], 'Set NULL did not persist SQL NULL');
  webview.statusCell._listeners.contextmenu({ preventDefault() {}, clientX: 20, clientY: 20 });
  webview.getElement('#cellSqlExpression').value = 'now()';
  webview.getElement('#cellValueMenu')._listeners.click({ target: { closest: () => ({ dataset: { cellAction: 'expression' } }) } });
  webview.flushTimers();
  await Promise.all(extensionOperations);
  const nowResult = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', 'SELECT metadata FROM orders WHERE id = 4', 'execute'
  ], { encoding: 'utf8' }));
  assert(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(nowResult.rows[0][0]), 'SQL expression now() did not persist as a timestamp: ' + JSON.stringify(nowResult.rows));
  webview.statusCell.dataset.col = '2';
  webview.statusCell.textContent = 'NULL';
  webview.statusCell._listeners.blur();
  webview.flushTimers();
  await Promise.all(extensionOperations);
  const literalNullResult = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', 'SELECT status FROM orders WHERE id = 4', 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(literalNullResult.rows, [['NULL']], 'typing NULL into a text cell did not preserve the literal text');
  webview.statusCell.textContent = 'shipped';
  webview.statusCell._listeners.input();

  webview.getElement('#inlineSql').value = 'SELECT id, status FROM orders WHERE id = 4';
  webview.getElement('#runInlineSql')._listeners.click();
  assert(!webview.postedMessages.some(message => message.type === 'runSql'), 'SQL ran before the pending edit was committed');
  await waitFor(() => webview.postedMessages.some(message => message.type === 'runSql'), 'queued SQL execution');
  await waitFor(() => webview.getElement('#dataSummary').textContent.startsWith('1 rows') && webview.getElement('#dataResult').innerHTML.includes('shipped'), 'updated SQL result');
  const run = webview.postedMessages.find(message => message.type === 'runSql');
  assert(run, 'SQL Run message was not sent after the save');
  assert(webview.postedMessages.indexOf(run) > webview.postedMessages.indexOf(save), 'SQL Run was not ordered after the save request');

  const sqlGrid = webview.getElement('#dataResult').innerHTML;
  assert(sqlGrid.includes('shipped'), 'SQL rows did not render in the main grid: ' + sqlGrid);
  assert(webview.getElement('#dataSummary').textContent.startsWith('1 rows'), 'grid summary did not switch to SQL result');
  assert(webview.getElement('#inlineSqlResults').innerHTML.includes('data-query-export'), 'query result actions were not rendered');

  webview.getElement('#inlineSql').value = 'SELECT * FROM "orders" LIMIT 200 order by total desc;';
  webview.getElement('#runInlineSql')._listeners.click();
  await waitFor(() => webview.getElement('#inlineSqlResults').innerHTML.includes('unexpected token'), 'SQL syntax error shown in the editor');
  assert(webview.getElement('#sqlStatus').textContent.includes('failed'), 'SQL editor did not mark the malformed statement as failed');
  const historyRerun = webview.dataRerunButtons().find(button => button.dataset.rerun === 'SELECT id, status FROM orders WHERE id = 4');
  assert(historyRerun && historyRerun._listeners.click, 'query history did not expose a bound rerun action');
  const queryCountBeforeHistoryRerun = webview.postedMessages.filter(message => message.type === 'query').length;
  const sqlRunCountBeforeHistoryRerun = webview.postedMessages.filter(message => message.type === 'runSql').length;
  historyRerun._listeners.click();
  await waitFor(() => webview.postedMessages.filter(message => message.type === 'runSql').length > sqlRunCountBeforeHistoryRerun && !webview.getElement('#inlineSqlResults').hidden && webview.getElement('#inlineSqlResults').innerHTML.includes('data-query-export'), 'history query reruns in the workbench grid');
  assert.strictEqual(webview.getElement('#inlineSql').value, 'SELECT id, status FROM orders WHERE id = 4', 'history rerun did not load the saved SQL into the editor');
  assert.strictEqual(webview.postedMessages.filter(message => message.type === 'query').length, queryCountBeforeHistoryRerun, 'history rerun opened a separate query tab');

  webview.dispatch({ type: 'preview', source: 'table', name: 'orders', requestId: selection.requestId, preview: { columns: ['id'], rows: [[1], [2], [3], [4]] } });
  assert.strictEqual(webview.getElement('#dataResult').innerHTML, sqlGrid, 'stale table response overwrote the SQL grid');
  assert.strictEqual(webview.getElement('#inlineSqlResults').hidden, false, 'stale table response hid SQL result actions');

  webview.getElement('.explorer')._listeners.click({
    target: { closest: () => ({ dataset: { name: 'accounts' } }) }
  });
  await waitFor(() => webview.getElement('#dataResult').innerHTML.includes('data-sort-col'), 'another table selected before CSV import');
  webview.getElement('.explorer')._listeners.click({
    target: { closest: () => ({ dataset: { name: 'orders' } }) }
  });
  await waitFor(() => webview.getElement('#inlineSql').value.includes('FROM "orders"') && webview.getElement('#dataResult').innerHTML.includes('data-sort-col'), 'orders table selected for CSV import');
  webview.getElement('#dataFilter').value = "status = 'review'";
  webview.getElement('#applyFilter')._listeners.click();
  assert(webview.postedMessages.some(message => message.type === 'browse' && message.where === "status = 'review'"), 'table filter did not request filtered rows');
  await waitFor(() => (webview.getElement('#dataSummary').textContent.startsWith('2 rows') && webview.getElement('#dataResult').innerHTML.includes('review')) || webview.getElement('#dataSummary').textContent.startsWith('Could not load') || operationErrors.length > 0, 'filtered table rows or visible filter error');
  assert(webview.getElement('#dataSummary').textContent.startsWith('2 rows'), 'table filter failed: ' + webview.getElement('#dataSummary').textContent + ' / ' + JSON.stringify(webview.dispatchedMessages.filter(message => message.type === 'browseResult').slice(-1)) + ' / ' + operationErrors.map(error => error.stack || error.message).join('\n'));
  webview.getElement('#dataFilter').value = '';
  webview.getElement('#applyFilter')._listeners.click();
  await waitFor(() => webview.getElement('#dataSummary').textContent.startsWith('4 rows'), 'cleared table filter');
  webview.flushTimers();
  const idSort = webview.dataSortButtons().find(button => button.dataset.sortCol === 'id');
  assert(idSort && idSort._listeners.click, 'table column sort action was not bound');
  idSort._listeners.click();
  await waitFor(() => webview.getElement('#dataResult').innerHTML.includes('id ↑') && webview.getElement('#dataSummary').textContent.startsWith('4 rows'), 'ascending table sort');
  webview.flushTimers();
  const descendingIdSort = webview.dataSortButtons().find(button => button.dataset.sortCol === 'id');
  descendingIdSort._listeners.click();
  await waitFor(() => webview.getElement('#dataResult').innerHTML.includes('id ↓') && webview.getElement('#dataSummary').textContent.startsWith('4 rows'), 'descending table sort');
  const firstSortedRow = webview.getElement('#dataResult').innerHTML.match(/<tbody><tr[^>]*>([\s\S]*?)<\/tr>/);
  assert(firstSortedRow && /<td[^>]*>4<\/td>/.test(firstSortedRow[1]), 'descending table sort did not put the largest id first: ' + (firstSortedRow && firstSortedRow[1]));
  webview.flushTimers();
  const ascendingIdSort = webview.dataSortButtons().find(button => button.dataset.sortCol === 'id');
  ascendingIdSort._listeners.click();
  await waitFor(() => webview.getElement('#dataResult').innerHTML.includes('id ↑') && webview.getElement('#dataSummary').textContent.startsWith('4 rows'), 'table sort reset to ascending');

  webview.getElement('#importCsvFile').files = [{ name: 'orders.csv', text: async () => 'id,account_id,status,total,created_at,metadata\n99,101,imported,5,2026-11-14T00:00:00Z,"{""source"":""csv""}"' }];
  await webview.getElement('#importCsvFile')._listeners.change();
  assert(webview.getElement('#csvImportPreview').innerHTML.includes('Import 1 rows'), 'CSV import preview did not render: ' + webview.getElement('#csvImportPreview').innerHTML);
  webview.getElement('#csvImportPreview')._listeners.click({
    target: { closest: () => ({ dataset: { csvImportAction: 'apply' } }) }
  });
  assert(webview.getElement('#addRow').disabled, 'row insertion remained enabled during CSV import');
  assert(webview.getElement('#deleteRows').disabled, 'row deletion remained enabled during CSV import');
  assert(!webview.getElement('#dataResult').innerHTML.includes('contenteditable="true"'), 'cell editing remained enabled during CSV import');
  await waitFor(() => webview.getElement('#dataSummary').textContent.startsWith('Imported 1 row') && webview.getElement('#dataResult').innerHTML.includes('imported'), 'CSV import result refreshed in grid');

  const makeColumnInput = (value, checked = false) => ({ value, checked });
  const createColumns = [
    { name: 'id', type: 'INTEGER', defaultValue: '', primaryKey: true, notNull: false, unique: false },
    { name: 'status', type: 'TEXT', defaultValue: "'new'", primaryKey: false, notNull: true, unique: false }
  ].map(column => ({
    querySelector(selector) {
      const fields = {
        '[data-create-name]': makeColumnInput(column.name),
        '[data-create-type]': makeColumnInput(column.type),
        '[data-create-default]': makeColumnInput(column.defaultValue),
        '[data-create-primary]': makeColumnInput('', column.primaryKey),
        '[data-create-not-null]': makeColumnInput('', column.notNull),
        '[data-create-unique]': makeColumnInput('', column.unique)
      };
      return fields[selector];
    }
  }));
  webview.setCreateTableRows(createColumns);
  webview.getElement('#newTableName').value = 'workbench_smoke';
  webview.getElement('#previewCreateTable')._listeners.click();
  const createSql = webview.getElement('#manageSql').value;
  assert(createSql.includes('"id" INTEGER PRIMARY KEY') && createSql.includes('"status" TEXT NOT NULL DEFAULT \'new\''), 'table builder generated incomplete SQL');
  webview.getElement('#applySchema')._listeners.click();
  await waitFor(() => webview.getElement('#tableList').innerHTML.includes('workbench_smoke'), 'created table in sidebar');
  webview.getElement('.explorer')._listeners.click({
    target: { closest: () => ({ dataset: { name: 'workbench_smoke' } }) }
  });
  assert(webview.getElement('#addRow').disabled, 'row insertion remained enabled while loading the selected table');
  await waitFor(() => webview.getElement('#dataResult').innerHTML.includes('data-sort-col="status"') && !webview.getElement('#dataResult').innerHTML.includes('data-sort-col="account_id"'), 'created table data loaded for structure edits');

  webview.getElement('#addRow')._listeners.click();
  webview.flushTimers();
  const newStatusCell = webview.dataDraftInput();
  assert(newStatusCell, 'Add row did not render draft cells');
  newStatusCell.value = 'created from grid';
  newStatusCell._listeners.input();
  webview.getElement('#saveEdits')._listeners.click();
  const insertRequest = webview.postedMessages.filter(message => message.type === 'applyStatements' && message.source === 'data').at(-1);
  assert(insertRequest, 'Add row did not send a save request');
  assert(insertRequest.statements.some(statement => statement.includes('created from grid')), 'Add row save request omitted the draft value: ' + JSON.stringify(insertRequest) + ' / ' + webview.getElement('#dataSummary').textContent);
  await waitFor(() => webview.getElement('#dataResult').innerHTML.includes('created from grid'), 'added row appears in the table grid');
  const inserted = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', "SELECT status FROM workbench_smoke WHERE status = 'created from grid'", 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(inserted.rows, [['created from grid']], 'Add row was not persisted in the database: ' + JSON.stringify(inserted) + ' / ' + webview.getElement('#dataSummary').textContent);

  webview.flushTimers();
  const newRowCheckbox = webview.dataSelectionBoxes()[0];
  assert(newRowCheckbox && newRowCheckbox._listeners.change, 'row selection action was not bound');
  newRowCheckbox.checked = true;
  newRowCheckbox._listeners.change();
  assert.strictEqual(webview.getElement('#selectionCount').textContent, '1 selected', 'row selection count was not updated');
  webview.getElement('#deleteRows')._listeners.click();
  assert(webview.getElement('#saveEdits').querySelector('span').textContent.includes('1'), 'delete was not staged for saving');
  webview.getElement('#saveEdits')._listeners.click();
  await waitFor(() => !webview.getElement('#dataResult').innerHTML.includes('created from grid'), 'deleted row disappears from the table grid');
  const deleted = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', "SELECT status FROM workbench_smoke WHERE status = 'created from grid'", 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(deleted.rows || [], [], 'Delete row was not persisted in the database: ' + JSON.stringify(deleted));

  webview.getElement('#newColumnName').value = 'note';
  webview.getElement('#newColumnType').value = 'TEXT';
  webview.getElement('#previewAddColumn')._listeners.click();
  assert(webview.getElement('#manageSql').value.includes('ADD COLUMN "note" TEXT'), 'add-column control generated invalid SQL');
  webview.getElement('#applySchema')._listeners.click();
  await waitFor(() => webview.getElement('#structureView').innerHTML.includes('note') || webview.getElement('#dataSummary').textContent.startsWith('Save failed'), 'added column reflected in structure or failed visibly');
  assert(webview.getElement('#structureView').innerHTML.includes('note'), 'add column failed: ' + webview.getElement('#dataSummary').textContent);

  webview.getElement('#renameColumnSelect').value = 'note';
  webview.getElement('#renameColumnName').value = 'details';
  webview.getElement('#previewRenameColumn')._listeners.click();
  assert(webview.getElement('#manageSql').value.includes('RENAME COLUMN "note" TO "details"'), 'rename-column control generated invalid SQL');
  webview.getElement('#applySchema')._listeners.click();
  await waitFor(() => webview.getElement('#structureView').innerHTML.includes('details'), 'renamed column reflected in structure');

  webview.getElement('#indexColumns').selectedOptions = [{ value: 'status' }];
  webview.getElement('#indexName').value = 'idx_workbench_smoke_status';
  webview.getElement('#previewCreateIndex')._listeners.click();
  assert(webview.getElement('#manageSql').value.includes('CREATE INDEX IF NOT EXISTS "idx_workbench_smoke_status"'), 'index builder generated invalid SQL');
  webview.getElement('#applySchema')._listeners.click();
  await waitFor(() => webview.getElement('#structureView').innerHTML.includes('idx_workbench_smoke_status') || webview.getElement('#dataSummary').textContent.startsWith('Save failed'), 'created index reflected in structure or failed visibly');
  assert(webview.getElement('#structureView').innerHTML.includes('idx_workbench_smoke_status'), 'index creation failed: ' + webview.getElement('#dataSummary').textContent + ' / ' + webview.getElement('#manageSql').value);

  webview.getElement('#dropIndexSelect').value = 'idx_workbench_smoke_status';
  webview.getElement('#previewDropIndex')._listeners.click();
  assert(webview.getElement('#manageSql').value.includes('DROP INDEX IF EXISTS "idx_workbench_smoke_status"'), 'drop-index control generated invalid SQL');
  webview.getElement('#applySchema')._listeners.click();
  await waitFor(() => !webview.getElement('#structureView').innerHTML.includes('idx_workbench_smoke_status'), 'dropped index removed from structure');

  webview.getElement('#dropColumnSelect').value = 'details';
  webview.getElement('#previewDropColumn')._listeners.click();
  assert(webview.getElement('#manageSql').value.includes('DROP COLUMN "details"'), 'drop-column control generated invalid SQL');
  webview.getElement('#applySchema')._listeners.click();
  await waitFor(() => !webview.getElement('#structureView').innerHTML.includes('details'), 'dropped column removed from structure');

  const persisted = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', 'SELECT status FROM orders WHERE id = 4', 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(persisted.rows, [['shipped']], 'saved row update was not persisted in the database');
  const imported = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), [
    '-db', databasePath, '-sql', 'SELECT status FROM orders WHERE id = 99', 'execute'
  ], { encoding: 'utf8' }));
  assert.deepStrictEqual(imported.rows, [['imported']], 'CSV import was not persisted in the database');
  const schema = JSON.parse(childProcess.execFileSync(path.join(root, 'bin/cqlite-vscode-bridge'), ['-db', databasePath, 'schema'], { encoding: 'utf8' }));
  const smokeTable = schema.objects.find(object => object.name === 'workbench_smoke');
  assert(smokeTable, 'table builder DDL was not applied');
  assert(smokeTable.columns.some(column => column.name === 'status') && !smokeTable.columns.some(column => column.name === 'details'), 'column changes were not persisted');
  assert(!(smokeTable.indexes || []).some(index => index.name === 'idx_workbench_smoke_status'), 'index removal was not persisted');
  assert.strictEqual(operationErrors.length, 0, operationErrors.map(error => error.message).join('\n'));
  console.log('Example .cq opening and workbench data, query, import, and schema flows passed.');
}

async function waitFor(predicate, description) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  throw new Error('Timed out waiting for ' + description);
}

main().catch(error => {
  console.error(error.stack || error);
  process.exitCode = 1;
}).finally(() => {
  Module._load = originalLoad;
  try { childProcess.execFileSync('rm', ['-rf', tempRoot]); } catch (_) {}
});
