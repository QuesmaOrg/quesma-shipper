'use strict';

const key = 'fleet-manager-admin-credential';
let credential = sessionStorage.getItem(key) || '';
let configETag = '';
let createKind = '';
let organizations = [];
let currentOrg = '';
// What an organization created without a setting gets, as this deployment decided; read on connect.
let deploymentDefaults = {allow_quesma_etl: true};
// Detail cells wait on a second request, so a reload must be able to tell a late answer from a
// current one and drop it.
let tableGeneration = 0;
const installDetailCells = new Map();
const $ = (selector) => document.querySelector(selector);
const all = (selector, root = document) => [...root.querySelectorAll(selector)];
const expiryPicker = createExpiryPicker($('#create-dialog'));
let openHint = null;

function hideHint() {
  if (openHint?.matches(':popover-open')) openHint.hidePopover();
  openHint = null;
}

// Tooltips live in the browser's top layer, so revealing one never changes a
// table's overflow mode or resets its horizontal scroll position.
all('.hint').forEach((hint, index) => {
  const trigger = hint.querySelector('.hint-mark');
  const body = hint.querySelector('.hint-body');
  body.id = `hint-${index}`;
  trigger.setAttribute('aria-describedby', body.id);
  let hideTimer;
  const show = () => {
    clearTimeout(hideTimer);
    if (openHint !== body) hideHint();
    openHint = body;
    if (!body.matches(':popover-open')) body.showPopover();
    const anchor = trigger.getBoundingClientRect();
    const bounds = body.getBoundingClientRect();
    body.style.left = `${Math.max(8, Math.min(anchor.right - bounds.width, innerWidth - bounds.width - 8))}px`;
    body.style.top = `${Math.max(8, anchor.bottom + bounds.height + 16 <= innerHeight
      ? anchor.bottom + 8 : anchor.top - bounds.height - 8)}px`;
  };
  const scheduleHide = () => {
    hideTimer = setTimeout(() => {
      if (openHint === body && document.activeElement !== trigger) hideHint();
    }, 120);
  };
  hint.addEventListener('pointerenter', show);
  hint.addEventListener('pointerleave', scheduleHide);
  body.addEventListener('pointerenter', () => clearTimeout(hideTimer));
  body.addEventListener('pointerleave', scheduleHide);
  trigger.addEventListener('focus', show);
  trigger.addEventListener('blur', () => { if (openHint === body) hideHint(); });
  trigger.addEventListener('click', show);
});
document.addEventListener('scroll', hideHint, true);
window.addEventListener('resize', hideHint);
document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && openHint) { event.preventDefault(); hideHint(); }
});

function show(id) {
  hideHint();
  ['login', 'organizations', 'dashboard'].forEach((name) => $('#' + name).classList.toggle('hidden', name !== id));
  // The masthead stays; only what an unauthenticated visitor has no use for goes away.
  $('#organization-controls').classList.toggle('hidden', id === 'login');
}

function selectView(id) {
  hideHint();
  all('nav button').forEach((button) => button.classList.toggle('active', button.dataset.view === id));
  all('.view').forEach((view) => view.classList.toggle('hidden', view.id !== id));
}

function rememberCredential(page) {
  sessionStorage.setItem(key, credential);
  show(page);
}

function disconnect() {
  tableGeneration++;
  sessionStorage.removeItem(key);
  credential = '';
  organizations = [];
  currentOrg = '';
  $('#credential').value = '';
  $('#secret').textContent = '';
  if ($('#secret-dialog').open) $('#secret-dialog').close();
  show('login');
}

function toast(message) {
  const node = $('#toast');
  node.textContent = message;
  node.classList.add('show');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.classList.remove('show'), 3500);
}

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set('Authorization', 'Bearer ' + credential);
  if (options.body) headers.set('Content-Type', 'application/json');
  const response = await fetch('/v1/admin' + path, {...options, headers, cache: 'no-store', credentials: 'omit'});
  if (!response.ok) {
    const body = (await response.text()).trim();
    let message = body;
    try { message = JSON.parse(body).error || body; } catch (_) { /* Non-admin proxy errors may be text. */ }
    const error = new Error(message || `Request failed (${response.status})`);
    error.status = response.status;
    throw error;
  }
  return response;
}

function orgPath(path) {
  return `/orgs/${encodeURIComponent(currentOrg)}${path}`;
}

function configBody(form) {
  const data = new FormData(form);
  const body = {
    age_recipients: data.getAll('recipients').map((value) => String(value).trim()).filter(Boolean),
    include_install_recipient: false,
    allow_quesma_etl: data.get('quesma-etl') === 'on',
    authored_yaml: String(data.get('yaml') || '')
  };
  if (form.elements.display_name) body.display_name = String(data.get('display_name') || '').trim();
  return body;
}

// The create form starts from the deployment's defaults, not from a state written into the page, so
// what the administrator sees ticked is what an organization would have got by saying nothing.
function resetOrganizationForm() {
  const form = $('#organization-form');
  form.reset();
  setRecipients(form, []);
  form.elements['quesma-etl'].checked = deploymentDefaults.allow_quesma_etl !== false;
  syncQuesmaRecipient(form);
}

function fillConfig(config) {
  const form = $('#config-form');
  form.elements.display_name.value = config.display_name || config.organization;
  const current = organizations.find((org) => org.slug === currentOrg);
  if (current) current.display_name = form.elements.display_name.value;
  const option = [...$('#org-select').options].find((item) => item.value === currentOrg);
  if (option) option.textContent = form.elements.display_name.value;
  setRecipients(form, config.age_recipients);
  form.elements['quesma-etl'].checked = config.allow_quesma_etl !== false;
  syncQuesmaRecipient(form);
  form.elements.yaml.value = config.authored_yaml || '';
  $('#config-time').textContent = config.updated_at ? `Updated ${new Date(config.updated_at).toLocaleString()}` : '';
  lockConfig(true);
}

// The stored recipients decide who can decrypt future uploads, so the form is
// read-only until an administrator asks to edit it.
function lockConfig(locked) {
  const form = $('#config-form');
  form.classList.toggle('locked', locked);
  all('input[type="text"],input:not([type]),textarea', form).forEach((field) => { field.readOnly = locked; });
  form.elements['quesma-etl'].disabled = locked;
  all('.add-recipient,.remove-recipient', form).forEach((button) => { button.disabled = locked; });
  if (!locked) updateRecipientButtons(form.querySelector('[data-recipient-list]'));
  $('#edit-config').classList.toggle('hidden', !locked);
  $('#cancel-config').classList.toggle('hidden', locked);
  $('#apply-config').classList.toggle('hidden', locked);
}

function recipientRow(value = '') {
  const row = document.createElement('div');
  row.className = 'recipient-row';
  const input = document.createElement('input');
  input.name = 'recipients';
  input.required = true;
  input.placeholder = 'age1…';
  input.autocomplete = 'off';
  input.setAttribute('aria-label', 'Public age recipient');
  input.value = value;
  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'danger remove-recipient';
  remove.textContent = 'Remove';
  remove.addEventListener('click', () => {
    const list = row.parentElement;
    if (list.children.length <= 2) return;
    row.remove();
    updateRecipientButtons(list);
  });
  row.append(input, remove);
  return row;
}

function updateRecipientButtons(list) {
  const customerRows = all('.recipient-row:not(.quesma-recipient-row)', list);
  all('.remove-recipient', list).forEach((button) => { button.disabled = customerRows.length <= 2; });
}

function setRecipients(form, recipients) {
  const list = form.querySelector('[data-recipient-list]');
  const values = [...recipients];
  while (values.length < 2) values.push('');
  list.replaceChildren(...values.map(recipientRow));
  updateRecipientButtons(list);
}

function syncQuesmaRecipient(form) {
  const list = form.querySelector('[data-recipient-list]');
  const current = list.querySelector('.quesma-recipient-row');
  if (current) current.remove();
  if (!form.elements['quesma-etl'].checked) return;

  const row = document.createElement('div');
  row.className = 'recipient-row quesma-recipient-row';
  const input = document.createElement('input');
  input.readOnly = true;
  input.setAttribute('aria-label', 'Quesma ETL public age recipient');
  input.value = form.querySelector('.recipient-fieldset').dataset.quesmaEtlRecipient;
  row.append(input);
  list.append(row);
}

async function loadConfig() {
  const response = await api(orgPath('/config'));
  configETag = response.headers.get('ETag') || '';
  const config = await response.json();
  fillConfig(config);
  return config;
}

async function connect() {
  try {
    deploymentDefaults = await (await api('/defaults')).json();
    resetOrganizationForm();
    const response = await api('/orgs');
    organizations = await response.json();
    if (organizations.length === 0) {
      rememberCredential('organizations');
      return;
    }
    selectOrganization(organizations[0].slug);
    await loadConfig();
    rememberCredential('dashboard');
  } catch (error) {
    disconnect();
    toast(error.status === 401 ? 'Credential was not accepted.' : error.message);
    return;
  }
  try { await loadTables(); } catch (error) { toast(`Connected, but fleet lists could not be loaded: ${error.message}`); }
}

function selectOrganization(slug) {
  tableGeneration++;
  currentOrg = slug;
  const select = $('#org-select');
  select.replaceChildren(...organizations.map((org) => {
    const option = document.createElement('option'); option.value = org.slug; option.textContent = org.display_name; return option;
  }));
  select.value = slug;
}

function cell(row, value, className = '') {
  const node = row.insertCell();
  node.textContent = value || '—';
  if (className) node.className = className;
  return node;
}

function actionButton(label, handler) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'danger-solid';
  button.textContent = label;
  button.addEventListener('click', async () => {
    if (!window.confirm(`${label} this item?`)) return;
    button.disabled = true;
    try { await handler(); } catch (error) { toast(error.message); } finally { button.disabled = false; }
  });
  return button;
}

// actionButton is for the destructive ones: it confirms and is styled as a warning. Naming is
// neither, and is undone by naming again.
function quietButton(label, handler) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'quiet';
  button.textContent = label;
  button.addEventListener('click', async () => {
    button.disabled = true;
    try { await handler(); } catch (error) { toast(error.message); } finally { button.disabled = false; }
  });
  return button;
}

function emptyCell(row) {
  return row.insertCell();
}

function statusPill(text, revoked) {
  const node = document.createElement('span');
  // The word is the status; the class only lets a colour agree with it.
  node.className = `pill pill--${text.toLowerCase()}` + (revoked ? ' revoked' : '');
  node.textContent = text;
  return node;
}

async function mutate(path, message) {
  await api(orgPath(path), {method: 'POST'});
  toast(message);
  await loadTables();
}

function renderGrants(records) {
  const body = $('#grants tbody'); body.replaceChildren();
  $('#grants .table-empty').classList.toggle('hidden', records.length > 0);
  records.forEach((record) => {
    const row = body.insertRow(); cell(row, record.id); cell(row, new Date(record.expires_at).toLocaleString());
    const revoked = Boolean(record.revoked_at); const expired = new Date(record.expires_at) <= new Date();
    emptyCell(row).append(statusPill(revoked ? 'Revoked' : expired ? 'Expired' : 'Active', revoked));
    const actions = emptyCell(row); if (!revoked) actions.append(actionButton('Revoke', () => mutate(`/grants/${encodeURIComponent(record.id)}/revoke`, 'Grant revoked')));
  });
}

function renderInvites(records) {
  const body = $('#invites tbody'); body.replaceChildren();
  $('#invites .table-empty').classList.toggle('hidden', records.length > 0);
  records.forEach((record) => {
    const row = body.insertRow(); cell(row, record.id); cell(row, new Date(record.expires_at).toLocaleString());
    let status = 'Active'; if (record.revoked_at) status = 'Revoked'; else if (record.spent_at) status = 'Spent'; else if (record.reserved_install_id) status = 'Reserved'; else if (new Date(record.expires_at) <= new Date()) status = 'Expired';
    emptyCell(row).append(statusPill(status, status === 'Revoked'));
    const actions = emptyCell(row);
    if (!record.revoked_at) actions.append(actionButton('Revoke', () => mutate(`/invites/${encodeURIComponent(record.id)}/revoke`, 'Invite revoked')));
    if (record.reserved_install_id && !record.spent_at) actions.append(actionButton('Release', () => mutate(`/invites/${encodeURIComponent(record.id)}/release`, 'Reservation released')));
  });
}

function stamp(value) {
  const date = new Date(value);
  return value && !Number.isNaN(date.valueOf()) ? date.toLocaleString() : '';
}

function renderInstalls(records) {
  const body = $('#installs tbody'); body.replaceChildren();
  $('#installs .table-empty').classList.toggle('hidden', records.length > 0);
  installDetailCells.clear();
  records.forEach((record) => {
    const row = body.insertRow(); cell(row, record.install_id);
    const name = cell(row, '');
    cell(row, record.hostname); cell(row, record.platform);
    const os = cell(row, ''), booted = cell(row, '');
    cell(row, stamp(record.created_at));
    const config = cell(row, ''), vend = cell(row, ''), version = cell(row, '');
    installDetailCells.set(record.install_id, {name, os, booted, config, vend, version});
    const revoked = record.status === 'revoked'; emptyCell(row).append(statusPill(record.status, revoked));
    const actions = emptyCell(row);
    // A revoked install keeps its Name button: the objects it already wrote still want a label.
    actions.append(quietButton('Name', () => openRename(record.install_id, name.textContent)));
    if (!revoked) actions.append(actionButton('Revoke', () => mutate(`/installs/${encodeURIComponent(record.install_id)}/revoke`, 'Install revoked')));
  });
}

// One request per fleet, not per install, and off the critical path: the table is already on
// screen while this fills its detail columns in.
async function loadSeen(generation) {
  const records = await (await api(orgPath('/installs/seen'))).json();
  if (generation !== tableGeneration) return;
  records.forEach((record) => {
    const cells = installDetailCells.get(record.install_id);
    if (!cells) return;
    cells.os.textContent = record.os || '—';
    cells.booted.textContent = stamp(record.booted_at) || '—';
    cells.config.textContent = stamp(record.last_config_at) || '—';
    cells.vend.textContent = stamp(record.last_vend_at) || '—';
    cells.version.textContent = record.client_version || '—';
  });
}

// Same shape as loadSeen, and separate from it: a name is written by an administrator and read
// from the install's own root, while telemetry is written by the shipper into control/.
async function loadTags(generation) {
  const records = await (await api(orgPath('/installs/tags'))).json();
  if (generation !== tableGeneration) return;
  records.forEach((record) => {
    const cells = installDetailCells.get(record.install_id);
    if (cells) cells.name.textContent = record.name || '—';
  });
}

async function loadTables() {
  const generation = ++tableGeneration;
  const [grants, invites, installs] = await Promise.all(['/grants', '/invites', '/installs'].map(async (path) => (await api(orgPath(path))).json()));
  if (generation !== tableGeneration) return;
  renderGrants(grants); renderInvites(invites); renderInstalls(installs);
  loadSeen(generation).catch((error) => {
    if (generation === tableGeneration) toast(`Install details unavailable: ${error.message}`);
  });
  loadTags(generation).catch((error) => {
    if (generation === tableGeneration) toast(`Install names unavailable: ${error.message}`);
  });
}

$('#login-form').addEventListener('submit', (event) => { event.preventDefault(); credential = $('#credential').value.trim(); connect(); });
$('#home').addEventListener('click', (event) => {
  if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  selectView('configuration');
  show(credential ? (currentOrg ? 'dashboard' : 'organizations') : 'login');
  window.scrollTo({top: 0, left: 0});
});
$('#logout').addEventListener('click', disconnect);
$('#org-select').addEventListener('change', async (event) => {
  selectOrganization(event.currentTarget.value);
  try { await loadConfig(); await loadTables(); } catch (error) { toast(error.message); }
});
$('#new-org').addEventListener('click', () => { $('#cancel-org').classList.toggle('hidden', organizations.length === 0); show('organizations'); });
$('#cancel-org').addEventListener('click', () => show('dashboard'));
$('#organization-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const form = event.currentTarget; const data = new FormData(form);
  const body = {...configBody(form), slug: String(data.get('slug')).trim(), display_name: String(data.get('display_name')).trim()};
  try {
    const response = await api('/orgs', {method: 'POST', body: JSON.stringify(body)});
    const org = await response.json(); organizations.push(org); organizations.sort((a, b) => a.slug.localeCompare(b.slug));
    selectOrganization(org.slug); resetOrganizationForm(); await loadConfig(); await loadTables(); show('dashboard'); toast('Organization created');
  } catch (error) { toast(error.message); }
});
$('#config-form').addEventListener('submit', async (event) => { event.preventDefault(); if (event.currentTarget.classList.contains('locked')) return; try { await api(orgPath('/config'), {method: 'PUT', headers: {'If-Match': configETag}, body: JSON.stringify(configBody(event.currentTarget))}); await loadConfig(); toast('Configuration applied'); } catch (error) { toast(error.status === 412 ? 'Configuration changed elsewhere. Refresh and try again.' : error.message); } });

$('#edit-config').addEventListener('click', () => { lockConfig(false); $('#config-form').elements.display_name.focus(); });
$('#cancel-config').addEventListener('click', async () => { try { await loadConfig(); } catch (error) { toast(error.message); } });

all('nav button').forEach((button) => button.addEventListener('click', () => selectView(button.dataset.view)));

all('[data-create]').forEach((button) => button.addEventListener('click', () => {
  createKind = button.dataset.create;
  $('#create-title').textContent = createKind === 'grants' ? 'Create grant' : 'Create invite';
  expiryPicker.reset(new Date(Date.now() + 24 * 60 * 60 * 1000));
  $('#create-dialog').showModal();
}));
all('[data-close-create]').forEach((button) => button.addEventListener('click', () => $('#create-dialog').close()));
$('#create-dialog').addEventListener('close', () => expiryPicker.close());

all('[data-add-recipient]').forEach((button) => button.addEventListener('click', () => {
  const list = button.parentElement.querySelector('[data-recipient-list]');
  const row = recipientRow();
  list.append(row);
  updateRecipientButtons(list);
  row.querySelector('input').focus();
}));

let pendingQuesmaDisable = null;
function requestQuesmaDisable(checkbox) {
  pendingQuesmaDisable = checkbox;
  $('#disable-quesma-dialog').showModal();
}
all('input[name="quesma-etl"]').forEach((checkbox) => checkbox.addEventListener('change', () => {
  if (checkbox.checked) {
    syncQuesmaRecipient(checkbox.form);
    return;
  }
  checkbox.checked = true;
  requestQuesmaDisable(checkbox);
}));
$('#keep-quesma-etl').addEventListener('click', () => { pendingQuesmaDisable = null; $('#disable-quesma-dialog').close(); });
$('#confirm-disable-quesma').addEventListener('click', () => {
  if (pendingQuesmaDisable) {
    pendingQuesmaDisable.checked = false;
    syncQuesmaRecipient(pendingQuesmaDisable.form);
  }
  pendingQuesmaDisable = null;
  $('#disable-quesma-dialog').close();
});

$('#create-dialog form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const expiresAt = expiryPicker.value(); if (!expiresAt) return;
  const button = $('#confirm-create'); button.disabled = true;
  try {
    const response = await api(orgPath('/' + createKind), {method: 'POST', body: JSON.stringify({expires_at: expiresAt.toISOString()})});
    const result = await response.json();
    $('#create-dialog').close();
    $('#secret').textContent = result.secret;
    $('#enroll-command').textContent = `quesma-shipper login --server ${location.origin} ${result.secret}`;
    $('#secret-dialog').showModal();
    await loadTables();
  } catch (error) { toast(error.message); } finally { button.disabled = false; }
});
let renameInstall = null;
function openRename(installID, current) {
  renameInstall = installID;
  $('#rename-install').textContent = installID;
  $('#rename-name').value = current === '—' ? '' : current;
  $('#rename-dialog').showModal();
  $('#rename-name').focus();
}
$('#confirm-rename').addEventListener('click', async (event) => {
  event.preventDefault();
  const input = $('#rename-name'); if (!input.reportValidity()) return;
  const button = event.currentTarget; button.disabled = true;
  try {
    await api(orgPath(`/installs/${encodeURIComponent(renameInstall)}/tags`), {method: 'PUT', body: JSON.stringify({name: input.value})});
    $('#rename-dialog').close();
    toast(input.value ? 'Install named' : 'Name cleared');
    await loadTables();
  } catch (error) { toast(error.message); } finally { button.disabled = false; }
});
$('#rename-dialog').addEventListener('close', () => { renameInstall = null; $('#rename-name').value = ''; });

$('#copy-secret').addEventListener('click', async () => { try { await navigator.clipboard.writeText($('#secret').textContent); toast('Secret copied'); } catch (error) { toast(`Copy failed: ${error.message}`); } });
$('#copy-command').addEventListener('click', async () => { try { await navigator.clipboard.writeText($('#enroll-command').textContent); toast('Command copied'); } catch (error) { toast(`Copy failed: ${error.message}`); } });
all('[data-copy-keygen]').forEach((button) => button.addEventListener('click', async () => { try { await navigator.clipboard.writeText(button.parentElement.querySelector('code').textContent); toast('Command copied'); } catch (error) { toast(`Copy failed: ${error.message}`); } }));
$('#close-secret').addEventListener('click', () => $('#secret-dialog').close());
$('#secret-dialog').addEventListener('close', () => { $('#secret').textContent = ''; $('#enroll-command').textContent = ''; });

all('[data-server]').forEach((node) => { node.textContent = location.origin; });
resetOrganizationForm();
if (credential) connect(); else show('login');
