const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const sandbox = {structuredClone, document: {addEventListener() {}}};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(path.join(__dirname, '../src/admin-ui/collection.js'), 'utf8'), sandbox);

function form(overrides = {}) {
  return {
    elements: Object.fromEntries(['schedule', 'max_files_per_run', 'drain_deadline', 'rule_packs', 'secret_key_names'].map((name) => [name, {value: overrides[name] || ''}])),
    querySelectorAll: () => [],
  };
}

test('collection form keeps advanced source settings when editing supported fields', () => {
  const source = {id: 'claude-code', roots: ['~/logs'], include: ['*.jsonl'], max_file_bytes: 123, enrichers: {'join': false}};
  const row = {_source: source, querySelector: (selector) => ({value: {'[data-source-id]': 'claude-code', '[data-source-enabled]': 'false', '[data-source-exclude]': 'private/**\n'}[selector]})};
  const fields = form({schedule: '2m', max_files_per_run: '17', drain_deadline: '30s'});
  fields.querySelectorAll = () => [row];
  const result = JSON.parse(JSON.stringify(sandbox.collectionBody(fields)));
  assert.deepEqual(result.sources, [{...source, enabled: false, exclude: ['private/**']}]);
  assert.equal(result.max_files_per_run, 17);
  assert.deepEqual(result.mode, {schedule: '2m'});
});

test('invalid legacy config cannot be silently replaced by empty form defaults', () => {
  const fields = form();
  fields._collectionError = 'Stored collection requires correction';
  assert.throws(() => sandbox.collectionBody(fields), /requires correction/);
});
