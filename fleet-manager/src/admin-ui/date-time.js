'use strict';

function formatLocalDate(date) {
  return [date.getDate(), date.getMonth() + 1, date.getFullYear()]
    .map((value, index) => String(value).padStart(index === 2 ? 4 : 2, '0')).join('/');
}

// Round-trip every part: Date otherwise silently turns 31 February or a missing
// daylight-saving hour into a different expiry than the administrator entered.
function parseLocalDateTime(day, time) {
  const dateParts = /^(\d{2})\/(\d{2})\/(\d{4})$/.exec(day);
  const timeParts = /^(\d{2}):(\d{2})$/.exec(time);
  if (!dateParts || !timeParts) return null;
  const [, d, m, y] = dateParts.map(Number);
  const [, h, min] = timeParts.map(Number);
  if (y < 1000 || m < 1 || m > 12 || d < 1 || d > 31 || h > 23 || min > 59) return null;
  const value = new Date(y, m - 1, d, h, min);
  return value.getFullYear() === y && value.getMonth() === m - 1 && value.getDate() === d
    && value.getHours() === h && value.getMinutes() === min ? value : null;
}

function shiftCalendarMonth(date, months) {
  const target = new Date(date.getFullYear(), date.getMonth() + months, 1, 12);
  const last = new Date(target.getFullYear(), target.getMonth() + 1, 0).getDate();
  target.setDate(Math.min(date.getDate(), last));
  return target;
}

function createExpiryPicker(root) {
  const dateInput = root.querySelector('[data-expiry-date]');
  const timeInput = root.querySelector('[data-expiry-time]');
  const toggle = root.querySelector('[data-calendar-toggle]');
  const calendar = root.querySelector('[data-calendar]');
  const title = root.querySelector('[data-calendar-month]');
  const days = root.querySelector('[data-calendar-days]');
  const fullDate = new Intl.DateTimeFormat('en-GB', {dateStyle: 'full'});
  let cursor = new Date();

  root.querySelector('[data-expiry-zone]').textContent =
    `Local time (${Intl.DateTimeFormat().resolvedOptions().timeZone}).`;

  const isOpen = () => calendar.matches(':popover-open');

  function close(focus = false) {
    if (isOpen()) calendar.hidePopover();
    toggle.setAttribute('aria-expanded', 'false');
    if (focus) toggle.focus({preventScroll: true});
  }

  // The top-layer popup can extend beyond the dialog without resizing it.
  // Keep it anchored to the date field, flipping above when space is tighter below.
  function positionCalendar() {
    if (!isOpen()) return;
    const viewport = window.visualViewport;
    const width = viewport?.width ?? innerWidth;
    const height = viewport?.height ?? innerHeight;
    const left = (viewport?.offsetLeft ?? 0) + 8;
    const top = (viewport?.offsetTop ?? 0) + 8;
    const right = left + width - 16;
    const bottom = top + height - 16;
    const anchor = dateInput.getBoundingClientRect();
    if (anchor.bottom < top || anchor.top > bottom) { close(true); return; }

    calendar.style.maxWidth = `${width - 16}px`;
    calendar.style.maxHeight = `${height - 16}px`;
    const bounds = calendar.getBoundingClientRect();
    const below = bottom - anchor.bottom - 8;
    const above = anchor.top - top - 8;
    const openBelow = below >= bounds.height || below >= above;
    const available = Math.max(0, openBelow ? below : above);
    calendar.style.maxHeight = `${available}px`;
    calendar.style.left = `${Math.max(left, Math.min(anchor.left, right - bounds.width))}px`;
    calendar.style.top = `${openBelow ? anchor.bottom + 8 : anchor.top - 8 - Math.min(bounds.height, available)}px`;
  }

  function select(date) {
    dateInput.value = formatLocalDate(date);
    dateInput.setCustomValidity('');
    toggle.setAttribute('aria-label', `Choose expiry date, ${fullDate.format(date)}`);
    close(true);
  }

  function render(focus = false) {
    title.textContent = cursor.toLocaleDateString('en-GB', {month: 'long', year: 'numeric'});
    const selected = dateInput.value;
    const today = formatLocalDate(new Date());
    const first = new Date(cursor.getFullYear(), cursor.getMonth(), 1, 12);
    first.setDate(first.getDate() - (first.getDay() + 6) % 7);
    const rows = [];
    for (let week = 0; week < 6; week++) {
      const row = document.createElement('tr');
      for (let day = 0; day < 7; day++) {
        const date = new Date(first);
        date.setDate(first.getDate() + week * 7 + day);
        const cell = row.insertCell();
        const button = document.createElement('button');
        button.type = 'button';
        button.textContent = date.getDate();
        button.dataset.date = formatLocalDate(date);
        button.setAttribute('aria-label', fullDate.format(date));
        button.tabIndex = button.dataset.date === formatLocalDate(cursor) ? 0 : -1;
        if (button.dataset.date === selected) cell.setAttribute('aria-selected', 'true');
        if (button.dataset.date === today) button.setAttribute('aria-current', 'date');
        if (date.getMonth() !== cursor.getMonth()) button.classList.add('calendar-adjacent');
        button.addEventListener('click', () => select(date));
        cell.append(button);
      }
      rows.push(row);
    }
    days.replaceChildren(...rows);
    if (focus) days.querySelector('[tabindex="0"]').focus();
  }

  toggle.addEventListener('click', () => {
    if (isOpen()) { close(); return; }
    cursor = parseLocalDateTime(dateInput.value, '12:00') || new Date();
    render();
    calendar.showPopover({source: toggle});
    toggle.setAttribute('aria-expanded', 'true');
    positionCalendar();
    if (isOpen()) days.querySelector('[tabindex="0"]').focus();
  });

  document.addEventListener('pointerdown', (event) => {
    if (isOpen() && !calendar.contains(event.target) && !toggle.contains(event.target)) close(true);
  });
  document.addEventListener('focusin', (event) => {
    if (isOpen() && !calendar.contains(event.target) && !toggle.contains(event.target)) close();
  });
  document.addEventListener('scroll', (event) => {
    if (!calendar.contains(event.target)) positionCalendar();
  }, true);
  window.addEventListener('resize', positionCalendar);
  window.visualViewport?.addEventListener('resize', positionCalendar);
  window.visualViewport?.addEventListener('scroll', positionCalendar);

  root.querySelectorAll('[data-calendar-step]').forEach((button) => {
    button.addEventListener('click', () => {
      cursor = shiftCalendarMonth(cursor, Number(button.dataset.calendarStep));
      render();
    });
  });
  root.querySelector('[data-calendar-today]').addEventListener('click', () => select(new Date()));
  root.querySelector('[data-calendar-close]').addEventListener('click', () => close(true));

  days.addEventListener('keydown', (event) => {
    const button = event.target.closest('button[data-date]');
    if (!button) return;
    const date = parseLocalDateTime(button.dataset.date, '12:00');
    const weekday = (date.getDay() + 6) % 7;
    const offsets = {ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7, Home: -weekday, End: 6 - weekday};
    if (Object.hasOwn(offsets, event.key)) date.setDate(date.getDate() + offsets[event.key]);
    else if (event.key === 'PageUp' || event.key === 'PageDown') {
      cursor = shiftCalendarMonth(date, (event.key === 'PageUp' ? -1 : 1) * (event.shiftKey ? 12 : 1));
      event.preventDefault(); render(true); return;
    } else return;
    event.preventDefault(); cursor = date; render(true);
  });

  root.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && isOpen()) {
      event.preventDefault(); event.stopPropagation(); close(true);
    }
  });
  for (const input of [dateInput, timeInput]) {
    input.addEventListener('input', () => {
      dateInput.setCustomValidity(''); timeInput.setCustomValidity('');
    });
  }

  return {
    reset(date) {
      dateInput.value = formatLocalDate(date);
      timeInput.value = [date.getHours(), date.getMinutes()].map(value => String(value).padStart(2, '0')).join(':');
      dateInput.setCustomValidity(''); timeInput.setCustomValidity('');
      toggle.setAttribute('aria-label', `Choose expiry date, ${fullDate.format(date)}`);
      close();
    },
    value() {
      dateInput.setCustomValidity(''); timeInput.setCustomValidity('');
      if (!dateInput.reportValidity() || !timeInput.reportValidity()) return null;
      if (!parseLocalDateTime(dateInput.value, '12:00')) {
        dateInput.setCustomValidity('Enter a valid date as DD/MM/YYYY.');
        dateInput.reportValidity(); return null;
      }
      const date = parseLocalDateTime(dateInput.value, timeInput.value);
      if (!date) {
        timeInput.setCustomValidity('Enter a valid local time as HH:MM (00:00–23:59).');
        timeInput.reportValidity(); return null;
      }
      if (date <= new Date()) {
        dateInput.setCustomValidity('Choose an expiry in the future.');
        dateInput.reportValidity(); return null;
      }
      return date;
    },
    close
  };
}
