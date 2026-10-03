'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {parseInventoryCSV} = require('../src/admin-ui/metadata-import.js');

test('inventory CSV preserves exact hostnames and quoted field values', () => {
  assert.deepEqual(parseInventoryCSV('\uFEFFhostname,email,department\r\nAlice-Mac,alice@example.com,"R&D, Europe"\r\nAlice-Mac,bob@example.com,"Say ""hello"""'), [
    {hostname: 'Alice-Mac', metadata: {email: 'alice@example.com', department: 'R&D, Europe'}},
    {hostname: 'Alice-Mac', metadata: {email: 'bob@example.com', department: 'Say "hello"'}},
  ]);
});

test('malformed inventory never reaches the import API', () => {
  for (const text of [
    'host,email\na,a@example.com', 'hostname,email,email\na,b,c',
    'hostname,email\na', 'hostname,email\na,"unfinished',
    'hostname,email\na,"closed"oops', 'hostname,email\na,"two\nlines"',
    'hostname,email\na,' + 'a'.repeat(257),
  ]) assert.throws(() => parseInventoryCSV(text));
});
