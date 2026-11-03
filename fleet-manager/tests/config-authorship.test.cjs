'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

test('configuration view displays the response author and clears attribution on legacy responses', async () => {
  const label = {textContent: ''};
  const form = {
    elements: {
      display_name: {}, 'quesma-etl': {}, 'telemetry-enabled': {},
      telemetry_collector_url: {dataset: {}},
    },
    querySelector: () => ({textContent: ''}),
  };
  let author = 'partner';
  const config = {organization: 'acme', age_recipients: [], updated_at: '2026-11-03T12:00:00Z'};
  const sandbox = {
    organizations: [], currentOrg: 'acme', configETag: '',
    $: (selector) => ({'#config-form': form, '#org-select': {options: []}, '#config-time': label})[selector],
    setRecipients() {}, syncQuesmaRecipient() {}, syncTelemetry() {}, fillCollection() {}, lockConfig() {},
    orgPath: (value) => value,
    api: async () => ({headers: new Map([['Config-Updated-By', author], ['ETag', 'v1']]), json: async () => config}),
  };
  vm.createContext(sandbox);
  const source = fs.readFileSync(path.join(__dirname, '../src/admin-ui/app.js'), 'utf8');
  vm.runInContext(source.slice(source.indexOf('function fillConfig('), source.indexOf('function lockConfig(')), sandbox);
  vm.runInContext(source.slice(source.indexOf('async function loadConfig('), source.indexOf('async function connect(')), sandbox);
  for (const [value, expected] of [['partner', 'by partner'], ['administrator', 'by administrator'], ['unknown', '(author unknown)'], ['unavailable', '(author unavailable)'], [null, '(author unknown)']]) {
    author = value;
    await sandbox.loadConfig();
    assert.ok(label.textContent.startsWith('Updated '));
    assert.ok(label.textContent.endsWith(expected), label.textContent);
  }
});
