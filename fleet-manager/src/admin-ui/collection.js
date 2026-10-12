'use strict';

function fillCollection(form, collection, error = '', legacy = '') {
  form._collection = structuredClone(collection || {});
  form._collectionError = error;
  form.elements.schedule.value = collection?.mode?.schedule || '';
  form.elements.max_files_per_run.value = collection?.max_files_per_run ?? '';
  form.elements.drain_deadline.value = collection?.drain_deadline || '';
  form.elements.rule_packs.value = (collection?.scrub?.rule_packs || []).join('\n');
  form.elements.secret_key_names.value = (collection?.scrub?.secret_key_names || []).join('\n');
  form.querySelector('[data-collection-error]').textContent = error ? `${error} Rebuild the collection settings below to replace this invalid configuration; changes take effect only after Apply.` : '';
  const original = form.querySelector('[data-legacy-collection]');
  original.textContent = legacy;
  original.classList.toggle('hidden', !error);
  form.querySelector('[data-rebuild-collection]').classList.toggle('hidden', !error);
  const list = form.querySelector('[data-source-list]');
  list.replaceChildren();
  for (const source of collection?.sources || []) addCollectionSource(form, source);
}

// The sources a fleet's installs report are what has been discovered, not every source there is:
// an install that reports no catalog has unknown support, and a build nobody runs yet may carry a
// source no install reports. So the picker offers reported sources by name, keeps a stored ID nobody
// reports, and lets an administrator type an unreported ID with a warning. Counts are what shippers
// report they can collect, never what is enabled or installed on a machine.
function plural(count, one, many) {
  return `${count} ${count === 1 ? one : many}`;
}

function sourceSupport(catalog, id) {
  const source = (catalog.sources || []).find((item) => item.id === id);
  const supported = source ? source.installs : 0;
  return {source, supported, unsupported: catalog.installs.reporting - supported, unknown: catalog.installs.active - catalog.installs.reporting};
}

function supportText(catalog, id) {
  const support = sourceSupport(catalog, id);
  const parts = [support.source
    ? `${plural(support.supported, 'install reports', 'installs report')} it can collect this source`
    : 'Not reported by any install'];
  if (support.source && support.unsupported > 0) parts.push(`${plural(support.unsupported, 'reports', 'report')} it cannot`);
  if (support.unknown > 0) parts.push(`${plural(support.unknown, 'install has', 'installs have')} not reported what it can collect`);
  return parts.join(' · ');
}

function sourceLabel(source) {
  const family = source.family_name || source.family;
  return source.description ? `${family} — ${source.description} (${source.id})` : `${family} — ${source.id}`;
}

function sourceNote(text, warning = false) {
  const note = document.createElement('p');
  note.className = warning ? 'source-note source-warning' : 'source-note';
  note.textContent = text;
  return note;
}

function addCollectionSource(form, source = {}) {
  const row = document.createElement('fieldset');
  row._source = structuredClone(source);
  row.dataset.collectionSource = '';
  row.innerHTML = '<legend>Source override</legend><div data-source-field></div><label>Collection<select data-source-enabled><option value="">Use default</option><option value="true">Enabled</option><option value="false">Disabled</option></select></label><label>Additional exclusions (one per line)<textarea data-source-exclude></textarea></label><button type="button" data-remove-source class="quiet">Remove override</button>';
  const catalog = form._catalog;
  const field = row.querySelector('[data-source-field]');
  if (source.id) {
    // A stored override's source stays fixed and is kept whether or not any install reports it.
    const label = document.createElement('label');
    label.textContent = 'Source';
    const input = document.createElement('input');
    input.dataset.sourceId = '';
    input.value = source.id;
    input.readOnly = true;
    label.append(input);
    field.append(label);
    if (catalog) {
      const reported = sourceSupport(catalog, source.id).source;
      if (reported) field.append(sourceNote(sourceLabel(reported)));
      field.append(sourceNote(supportText(catalog, source.id)));
    }
  } else if (catalog) {
    sourcePicker(row, field, catalog);
  } else if (catalog === null) {
    // The reported sources could not be loaded, so whatever is typed here is unverified.
    unreportedSourceInput(row, field, 'Could not load the sources installs report, so no install has confirmed this ID. Check it before applying.');
  } else {
    // A new organization has no installs, so nothing reports and nothing is checked.
    const label = document.createElement('label');
    label.textContent = 'Source ID';
    const input = document.createElement('input');
    input.dataset.sourceId = '';
    input.required = true;
    label.append(input);
    field.append(label);
  }
  row.querySelector('[data-source-enabled]').value = source.enabled === undefined ? '' : String(source.enabled);
  row.querySelector('[data-source-exclude]').value = (source.exclude || []).join('\n');
  row.querySelector('[data-remove-source]').onclick = () => row.remove();
  form.querySelector('[data-source-list]').append(row);
}

function sourcePicker(row, field, catalog) {
  const label = document.createElement('label');
  label.textContent = 'Source';
  const select = document.createElement('select');
  select.dataset.sourceId = '';
  select.required = true;
  const prompt = document.createElement('option');
  prompt.value = '';
  prompt.textContent = catalog.sources.length ? 'Choose a source installs report' : 'No install reports its sources yet';
  select.append(prompt);
  const groups = new Map();
  for (const source of catalog.sources) {
    const family = source.family_name || source.family;
    if (!groups.has(family)) {
      const group = document.createElement('optgroup');
      group.label = family;
      groups.set(family, group);
      select.append(group);
    }
    const option = document.createElement('option');
    option.value = source.id;
    option.textContent = sourceLabel(source);
    groups.get(family).append(option);
  }
  label.append(select);
  const support = sourceNote('');
  select.onchange = () => { support.textContent = select.value ? supportText(catalog, select.value) : ''; };
  const unreported = document.createElement('button');
  unreported.type = 'button';
  unreported.className = 'quiet';
  unreported.dataset.sourceUnreported = '';
  unreported.textContent = 'Use an unreported source ID';
  unreported.onclick = () => {
    field.replaceChildren();
    unreportedSourceInput(row, field, 'No install has confirmed this ID. A build that does not have it leaves it out; check the ID before applying.');
  };
  field.append(label, support, unreported);
}

function unreportedSourceInput(row, field, warning) {
  row.dataset.unverified = '';
  const label = document.createElement('label');
  label.textContent = 'Unreported source ID';
  const input = document.createElement('input');
  input.dataset.sourceId = '';
  input.required = true;
  input.spellcheck = false;
  input.autocomplete = 'off';
  label.append(input);
  field.append(label, sourceNote(warning, true));
}

// unverifiedSources is what the administrator typed rather than picked: sent with the write as an
// acknowledgement that no install has confirmed these IDs. Fleet manager does not store it.
function unverifiedSources(form) {
  return [...form.querySelectorAll('[data-collection-source][data-unverified]')]
    .map((row) => row.querySelector('[data-source-id]').value.trim())
    .filter(Boolean);
}

function collectionBody(form) {
  if (form._collectionError) throw new Error(form._collectionError);
  const collection = structuredClone(form._collection || {});
  const schedule = form.elements.schedule.value.trim();
  if (schedule) collection.mode = {schedule}; else delete collection.mode;
  const max = form.elements.max_files_per_run.value;
  if (max !== '') collection.max_files_per_run = Number(max); else delete collection.max_files_per_run;
  const deadline = form.elements.drain_deadline.value.trim();
  if (deadline) collection.drain_deadline = deadline; else delete collection.drain_deadline;
  const lines = (value) => value.split('\n').map((s) => s.trim()).filter(Boolean);
  const preservedLines = (value, original = []) => value === original.join('\n') ? original : lines(value);
  const rulePacks = preservedLines(form.elements.rule_packs.value, collection.scrub?.rule_packs);
  const secretNames = preservedLines(form.elements.secret_key_names.value, collection.scrub?.secret_key_names);
  delete collection.scrub;
  if (rulePacks.length || secretNames.length) collection.scrub = {
    ...(rulePacks.length ? {rule_packs: rulePacks} : {}),
    ...(secretNames.length ? {secret_key_names: secretNames} : {})
  };
  const sources = [...form.querySelectorAll('[data-collection-source]')].map((row) => {
    const source = {...row._source, id: row.querySelector('[data-source-id]').value.trim()};
    const enabled = row.querySelector('[data-source-enabled]').value;
    if (enabled === '') delete source.enabled; else source.enabled = enabled === 'true';
    const exclude = preservedLines(row.querySelector('[data-source-exclude]').value, source.exclude);
    if (exclude.length) source.exclude = exclude; else delete source.exclude;
    return source;
  });
  if (sources.length) collection.sources = sources; else delete collection.sources;
  return collection;
}

document.addEventListener('click', (event) => {
  const rebuild = event.target.closest('[data-rebuild-collection]');
  if (rebuild) fillCollection(rebuild.closest('form'), {});
  const button = event.target.closest('[data-add-source]');
  if (button) addCollectionSource(button.closest('form'));
});
