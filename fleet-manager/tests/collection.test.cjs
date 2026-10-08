const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
// Just enough of the DOM for the source picker: elements, dataset attribute selectors and tag names.
// Elements the row template's innerHTML would create are stubbed on first lookup.
function matches(node, selector) {
  const attributes = [...selector.matchAll(/\[data-([a-z-]+)\]/g)].map((match) => match[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase()));
  return attributes.length ? attributes.every((name) => name in node.dataset) : node.tagName === selector.toUpperCase();
}
function descendants(node) {
  return [...node.children, ...Object.values(node.stubs)].flatMap((child) => [child, ...descendants(child)]);
}
function element(tag) {
  const node = {
    tagName: tag.toUpperCase(), dataset: {}, children: [], stubs: {}, value: '', textContent: '',
    append(...items) { node.children.push(...items); },
    replaceChildren(...items) { node.children = items; },
    set innerHTML(_) {},
    querySelector(selector) { return descendants(node).find((child) => matches(child, selector)) || (node.stubs[selector] ??= element('stub')); },
    querySelectorAll(selector) { return descendants(node).filter((child) => matches(child, selector)); },
  };
  return node;
}
const sandbox = {structuredClone, document: {addEventListener() {}, createElement: element}};
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

const catalog = {
  installs: {active: 14, reporting: 12},
  sources: [
    {id: 'claude-code-transcripts', family: 'claude-code', family_name: 'Claude Code', description: 'Session and subagent transcripts.', installs: 10},
    {id: 'codex-rollouts', family: 'codex', family_name: 'Codex', description: '', installs: 12},
  ],
  rule_packs: [], features: [],
};

function pickerForm(sourceCatalog) {
  const form = element('form');
  form._catalog = sourceCatalog;
  return form;
}

function texts(node) {
  return descendants(node).map((child) => child.textContent).filter(Boolean);
}

test('a new source is picked by name from what installs report, and stores the stable id', () => {
  const form = pickerForm(catalog);
  sandbox.addCollectionSource(form);
  const row = form.querySelectorAll('[data-collection-source]')[0];
  const select = row.querySelector('[data-source-id]');
  assert.equal(select.tagName, 'SELECT');
  const options = row.querySelectorAll('option').filter((option) => option.value);
  assert.deepEqual(options.map((option) => [option.value, option.textContent]), [
    ['claude-code-transcripts', 'Claude Code — Session and subagent transcripts. (claude-code-transcripts)'],
    ['codex-rollouts', 'Codex — codex-rollouts'],
  ]);
  assert.deepEqual(row.querySelectorAll('optgroup').map((group) => group.label), ['Claude Code', 'Codex']);
  select.value = 'claude-code-transcripts';
  select.onchange();
  assert.ok(texts(row).includes('10 installs report it can collect this source · 2 report it cannot · 2 installs have not reported what it can collect'));
  assert.equal(sandbox.collectionBody(Object.assign(form, {elements: fieldsOf()})).sources[0].id, 'claude-code-transcripts');
  assert.deepEqual(Array.from(sandbox.unverifiedSources(form)), []);
});

function fieldsOf() {
  return form().elements;
}

test('a stored source no install reports is kept and said to be unreported', () => {
  const form = pickerForm(catalog);
  sandbox.addCollectionSource(form, {id: 'custom-chats', enabled: false});
  const row = form.querySelectorAll('[data-collection-source]')[0];
  assert.equal(row.querySelector('[data-source-id]').value, 'custom-chats');
  assert.equal(row.querySelector('[data-source-id]').readOnly, true);
  assert.ok(texts(row).includes('Not reported by any install · 2 installs have not reported what it can collect'));
  assert.deepEqual(Array.from(sandbox.unverifiedSources(form)), []);
});

test('an unreported source ID is typed explicitly, warned about and sent as unverified', () => {
  const form = pickerForm(catalog);
  sandbox.addCollectionSource(form);
  const row = form.querySelectorAll('[data-collection-source]')[0];
  row.querySelector('[data-source-unreported]').onclick();
  const input = row.querySelector('[data-source-id]');
  assert.equal(input.tagName, 'INPUT');
  assert.ok(texts(row).some((text) => text.startsWith('No install has confirmed this ID.')));
  input.value = ' cursor-chats ';
  assert.deepEqual(Array.from(sandbox.unverifiedSources(form)), ['cursor-chats']);
});

test('without the reported sources the picker falls back to a plain, unverified input', () => {
  const form = pickerForm(null);
  sandbox.addCollectionSource(form);
  const row = form.querySelectorAll('[data-collection-source]')[0];
  assert.equal(row.querySelector('[data-source-id]').tagName, 'INPUT');
  row.querySelector('[data-source-id]').value = 'anything';
  assert.deepEqual(Array.from(sandbox.unverifiedSources(form)), ['anything']);
});

test('support is worded as what installs report, never as enabled, collecting or detected', () => {
  for (const id of ['claude-code-transcripts', 'codex-rollouts', 'unknown']) {
    assert.doesNotMatch(sandbox.supportText(catalog, id), /enabled|collecting|detected|installed/i);
  }
  assert.equal(sandbox.supportText({installs: {active: 1, reporting: 1}, sources: [{id: 'a', installs: 1}]}, 'a'), '1 install reports it can collect this source');
});
