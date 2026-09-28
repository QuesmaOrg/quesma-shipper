'use strict';

// Inventory exports use hostname plus metadata columns; quoted CSV fields may contain commas.
function parseInventoryCSV(text) {
  const rows = [];
  let row = [], value = '', quoted = false, closedQuote = false;
  text = text.replace(/^\uFEFF/, '').replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  for (let i = 0; i <= text.length; i++) {
    const char = text[i];
    if (quoted) {
      if (char === undefined) throw new Error('Unclosed quote in inventory CSV');
      if (char === '"' && text[i + 1] === '"') { value += '"'; i++; }
      else if (char === '"') { quoted = false; closedQuote = true; }
      else value += char;
    } else if (char === ',' || char === '\n' || char === undefined) {
      row.push(value); value = ''; closedQuote = false;
      if (char !== ',') {
        if (row.some((field) => field !== '')) rows.push(row);
        row = [];
      }
    } else if (char === '"' && value === '' && !closedQuote) {
      quoted = true;
    } else {
      if (closedQuote || char === '"') throw new Error('Invalid quote in inventory CSV');
      value += char;
    }
  }
  const headers = rows.shift() || [];
  if (headers[0] !== 'hostname' || headers.length < 2 || headers.length > 17 ||
      new Set(headers).size !== headers.length || headers.some((key) => !/^[a-z][a-z0-9_]{0,31}$/.test(key))) {
    throw new Error('Use hostname as the first column, followed by 1–16 unique metadata keys (lowercase letters, digits, underscores).');
  }
  if (!rows.length || rows.length > 1000) throw new Error('Import between 1 and 1000 inventory rows.');
  return rows.map((fields, index) => {
    if (fields.length !== headers.length || !fields[0]) throw new Error(`Row ${index + 1} must have a hostname and ${headers.length} columns.`);
    if (fields.some((field) => [...field].length > 256 || /\p{Cc}/u.test(field))) {
      throw new Error(`Row ${index + 1}: values must have at most 256 characters and no control characters.`);
    }
    return {hostname: fields[0], metadata: Object.fromEntries(headers.slice(1).map((key, i) => [key, fields[i + 1]]))};
  });
}

if (typeof module !== 'undefined') module.exports = {parseInventoryCSV};
