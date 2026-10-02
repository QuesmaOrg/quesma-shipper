// Run with the selected f1 head directory as the only argument.
const path = require('node:path');
const assert = require('node:assert/strict');
const {parseInventoryCSV} = require(path.resolve(process.argv[2], 'fleet-manager/src/admin-ui/metadata-import.js'));
const headers = ['hostname', ...Array.from({length: 16}, (_, i) => `field_${i}`)];
const csv = headers.join(',') + '\n' + Array.from({length: 1000}, (_, i) => [`host-${i}`, ...Array(16).fill('x'.repeat(60))].join(',')).join('\n');
const rows = parseInventoryCSV(csv);
const csvBytes = Buffer.byteLength(csv);
const jsonBytes = Buffer.byteLength(JSON.stringify({rows}));
assert.equal(rows.length, 1000);
assert(csvBytes <= 1048576);
assert(jsonBytes > 1048576);
console.log(JSON.stringify({rows: rows.length, csvBytes, jsonBytes, serverBodyLimit: 1048576}));
