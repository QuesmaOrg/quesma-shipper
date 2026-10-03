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

function addCollectionSource(form, source = {}) {
  const row = document.createElement('fieldset');
  row._source = structuredClone(source);
  row.dataset.collectionSource = '';
  row.innerHTML = '<legend>Source override</legend><label>Source ID<input data-source-id required></label><label>Collection<select data-source-enabled><option value="">Use default</option><option value="true">Enabled</option><option value="false">Disabled</option></select></label><label>Additional exclusions (one per line)<textarea data-source-exclude></textarea></label><button type="button" data-remove-source class="quiet">Remove override</button>';
  row.querySelector('[data-source-id]').value = source.id || '';
  row.querySelector('[data-source-id]').readOnly = Boolean(source.id);
  row.querySelector('[data-source-enabled]').value = source.enabled === undefined ? '' : String(source.enabled);
  row.querySelector('[data-source-exclude]').value = (source.exclude || []).join('\n');
  row.querySelector('[data-remove-source]').onclick = () => row.remove();
  form.querySelector('[data-source-list]').append(row);
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
