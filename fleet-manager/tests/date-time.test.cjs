const {test} = require('node:test');
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const {runInNewContext} = require('node:vm');

// The browser loads this as a classic script before app.js; exercise the same
// date functions without needing a DOM or a test-only production export.
const source = readFileSync(join(__dirname, '../src/admin-ui/date-time.js'), 'utf8');
const {parseLocalDateTime, formatLocalDate, shiftCalendarMonth} = runInNewContext(
  source + '\n({parseLocalDateTime, formatLocalDate, shiftCalendarMonth})', {Date},
);
process.env.TZ = 'Europe/Warsaw';

test('local expiry is converted to the correct UTC instant in summer and winter', () => {
  assert.equal(parseLocalDateTime('15/09/2026', '11:47').toISOString(), '2026-09-15T09:47:00.000Z');
  assert.equal(parseLocalDateTime('15/01/2027', '11:47').toISOString(), '2027-01-15T10:47:00.000Z');
  assert.equal(formatLocalDate(new Date(2026, 0, 1, 0, 5)), '01/01/2026');
});

test('invalid dates and times cannot silently become another expiry', () => {
  for (const [date, time] of [
    ['31/02/2026', '12:00'], ['29/02/2026', '12:00'], ['00/01/2026', '12:00'],
    ['01/13/2026', '12:00'], ['1/1/2026', '12:00'], ['2026-01-01', '12:00'],
    ['01/01/2026', '24:00'], ['01/01/2026', '12:60'], ['01/01/2026', '1:00'],
    ['', '12:00'], ['01/01/2026', ''], ['29/03/2026', '02:30'],
  ]) assert.equal(parseLocalDateTime(date, time), null, `${date} ${time}`);
  assert.equal(parseLocalDateTime('29/02/2028', '12:00').toISOString(), '2028-02-29T11:00:00.000Z');
});

test('calendar month navigation clamps month ends and crosses years', () => {
  assert.equal(formatLocalDate(shiftCalendarMonth(new Date(2026, 0, 31), 1)), '28/02/2026');
  assert.equal(formatLocalDate(shiftCalendarMonth(new Date(2028, 0, 31), 1)), '29/02/2028');
  assert.equal(formatLocalDate(shiftCalendarMonth(new Date(2026, 0, 31), -1)), '31/12/2025');
  assert.equal(formatLocalDate(shiftCalendarMonth(new Date(2028, 1, 29), 12)), '28/02/2029');
});
