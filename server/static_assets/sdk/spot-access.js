// Shared access picker for the platform pages: the deployer's access popover
// and the Mesh access dialog in /spots. It edits the two lists of an
// _access.json policy, visitors (allow) and maintainers, searching the mesh
// directory through /api/access/suggestions. Each page keeps its own policy
// semantics (what Clear means, when a policy is written) and drives the
// picker through the small API returned by createPicker.
(() => {
  'use strict';

  const POLICY_FIELDS = ['allow', 'maintainers', 'ai', 'slack', 'download'];
  const has = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

  const key = (entry) => entry.type + ':' + entry.value.toLowerCase();

  // entries turns policy strings into picker entries: values with "@" are
  // users, the rest are groups, matching the server's policy semantics.
  const entries = (values, userMeta) => {
    const out = [];
    for (const raw of values || []) {
      if (typeof raw !== 'string' || !raw.trim()) continue;
      const value = raw.trim();
      const entry = value.includes('@')
        ? { type: 'user', value, label: value, meta: userMeta || 'User' }
        : { type: 'group', value, label: value, meta: 'Group' };
      if (!out.some((e) => key(e) === key(entry))) out.push(entry);
    }
    return out;
  };

  const values = (list) => list.map((entry) => entry.value);

  // parsePolicy validates a policy object the way the server does (field
  // names case-insensitive, no duplicates or unknown fields, lists of
  // strings) and splits it into the two lists and the fields the picker
  // does not edit. It throws on an invalid policy.
  const parsePolicy = (data, userMeta) => {
    if (!data || typeof data !== 'object' || Array.isArray(data)) throw new Error('access policy must be a JSON object');
    const policy = {};
    for (const [name, value] of Object.entries(data)) {
      const normalized = name.toLowerCase();
      if (has(policy, normalized)) throw new Error('duplicate policy field');
      if (!POLICY_FIELDS.includes(normalized)) throw new Error('unknown policy field');
      policy[normalized] = value;
    }
    const list = (name) => {
      if (!has(policy, name)) return [];
      if (!Array.isArray(policy[name]) || policy[name].some((value) => typeof value !== 'string')) {
        throw new Error(name + ' must be an array of strings');
      }
      return policy[name];
    };
    const fields = {};
    for (const name of ['ai', 'slack', 'download']) {
      if (has(policy, name)) fields[name] = policy[name];
    }
    return {
      visitors: entries(list('allow'), userMeta),
      maintainers: entries(list('maintainers'), userMeta),
      fields,
      allowPresent: has(policy, 'allow'),
      maintainersPresent: has(policy, 'maintainers'),
    };
  };

  // matches reports whether an identity is named by a list of entries:
  // users by email, groups by name, both case-insensitive.
  const matches = (list, identity) => {
    if (!identity) return false;
    const email = (identity.email || '').toLowerCase();
    const groups = (identity.groups || []).map((group) => String(group).toLowerCase());
    return list.some((entry) => entry.type === 'user'
      ? email !== '' && entry.value.toLowerCase() === email
      : groups.includes(entry.value.toLowerCase()));
  };

  // createPicker wires a search box, a result list, a chip list and the
  // Visitors/Maintainers tabs. options.identity() returns the signed-in
  // identity, offered first as "You"; options.onChange(kind) runs after an
  // entry is toggled ('toggle') or the tab changes ('mode') and must call
  // render(); options.onEscape runs when Escape is pressed in the search box.
  const createPicker = (options) => {
    const { search, results, selected, visitorsTab, maintainersTab } = options;
    const idPrefix = options.idPrefix || 'access-opt-';
    const identity = options.identity || (() => null);
    const onChange = options.onChange || (() => {});
    const lists = { visitors: [], maintainers: [] };
    let mode = 'visitors';
    let directory = [];      // directory matches for the current query
    let optionButtons = [];  // currently rendered choice buttons
    let activeIndex = 0;     // keyboard-highlighted option
    let suggestTimer = null;
    let suggestSeq = 0;

    const current = () => lists[mode];
    const isSelected = (entry) => current().some((item) => key(item) === key(entry));

    const knownOptions = () => {
      const out = [];
      const me = identity();
      if (me && me.email) out.push({ type: 'user', value: me.email, label: me.email, meta: 'You' });
      for (const entry of directory) {
        if (!out.some((o) => key(o) === key(entry))) out.push(entry);
      }
      return out;
    };

    // The access directory (mesh users + groups) is searched server-side;
    // debounce so typing doesn't fire a request per key.
    const fetchSuggestions = (query) => {
      const seq = ++suggestSeq;
      clearTimeout(suggestTimer);
      if (!query) { directory = []; return; }
      suggestTimer = setTimeout(async () => {
        try {
          const res = await fetch('/api/access/suggestions?q=' + encodeURIComponent(query));
          if (!res.ok) return;
          const body = await res.json();
          if (seq !== suggestSeq) return; // a newer query already won
          directory = Array.isArray(body.suggestions) ? body.suggestions : [];
          renderResults();
        } catch {
          // directory unavailable (e.g. dev without a mesh API) — the
          // custom-email entry still lets you name someone.
        }
      }, 180);
    };

    const customEntry = (raw) => {
      const value = raw.trim();
      if (!value || !value.includes('@')) return null;
      return { type: 'user', value, label: value, meta: 'Email' };
    };

    const toggle = (entry) => {
      lists[mode] = isSelected(entry)
        ? current().filter((item) => key(item) !== key(entry))
        : [...current(), entry];
      onChange('toggle');
    };

    // applyActive reflects keyboard-navigation state: highlights the active
    // option, points aria-activedescendant at it, and keeps it in view.
    // Distinct from aria-selected, which marks listed entries.
    const applyActive = () => {
      optionButtons.forEach((b, i) => b.classList.toggle('active', i === activeIndex));
      const active = optionButtons[activeIndex];
      if (active) {
        search.setAttribute('aria-activedescendant', active.id);
        active.scrollIntoView({ block: 'nearest' });
      } else {
        search.removeAttribute('aria-activedescendant');
      }
    };

    const renderResults = () => {
      const query = search.value.trim().toLowerCase();
      let found = knownOptions().filter((entry) =>
        !query || entry.label.toLowerCase().includes(query) || entry.meta.toLowerCase().includes(query)
      );
      const custom = customEntry(search.value);
      if (custom && !found.some((entry) => key(entry) === key(custom))) found = [...found, custom];
      results.replaceChildren();
      optionButtons = [];
      if (!found.length) {
        activeIndex = -1;
        search.removeAttribute('aria-activedescendant');
        const empty = document.createElement('p');
        empty.className = 'accessnote';
        empty.textContent = query && !query.includes('@')
          ? 'No matching users or groups — type a full email to add one.'
          : 'No matching users.';
        results.appendChild(empty);
        return;
      }
      for (const entry of found) {
        const on = isSelected(entry);
        const button = document.createElement('button');
        button.type = 'button';
        button.id = idPrefix + optionButtons.length;
        button.className = 'access-choice';
        button.setAttribute('role', 'option');
        button.setAttribute('aria-selected', on ? 'true' : 'false');
        const icon = document.createElement('span');
        icon.className = 'choice-icon';
        icon.setAttribute('aria-hidden', 'true');
        icon.textContent = entry.type === 'group' ? '#' : '@';
        const main = document.createElement('span');
        main.className = 'choice-main';
        main.textContent = entry.label;
        const meta = document.createElement('span');
        meta.className = 'choice-meta';
        meta.textContent = entry.meta;
        main.appendChild(meta);
        const mark = document.createElement('span');
        mark.className = 'choice-mark';
        mark.textContent = on ? 'On' : '';
        button.append(icon, main, mark);
        // Stop the click bubbling to an outside-click handler: toggling
        // re-renders the list, detaching this node, which would otherwise
        // read as a click outside the picker.
        button.addEventListener('click', (e) => { e.stopPropagation(); toggle(entry); });
        results.appendChild(button);
        optionButtons.push(button);
      }
      if (activeIndex < 0 || activeIndex >= optionButtons.length) activeIndex = 0;
      applyActive();
    };

    const renderSelected = () => {
      selected.replaceChildren();
      for (const entry of current()) {
        const chip = document.createElement('span');
        chip.className = 'access-chip';
        const label = document.createElement('span');
        label.textContent = entry.label;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.setAttribute('aria-label', 'Remove ' + entry.label);
        remove.textContent = 'x';
        remove.addEventListener('click', (e) => { e.stopPropagation(); toggle(entry); });
        chip.append(label, remove);
        selected.appendChild(chip);
      }
    };

    const render = () => {
      visitorsTab.setAttribute('aria-selected', mode === 'visitors' ? 'true' : 'false');
      maintainersTab.setAttribute('aria-selected', mode === 'maintainers' ? 'true' : 'false');
      search.placeholder = mode === 'maintainers' ? 'Search maintainers' : 'Search visitors by email';
      renderResults();
      renderSelected();
    };

    const setMode = (next) => {
      mode = next;
      activeIndex = 0;
      onChange('mode');
    };

    visitorsTab.addEventListener('click', () => setMode('visitors'));
    maintainersTab.addEventListener('click', () => setMode('maintainers'));
    search.addEventListener('input', () => {
      activeIndex = 0; // highlight the first match as the list changes
      fetchSuggestions(search.value.trim());
      renderResults();
    });
    search.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (optionButtons.length) {
          activeIndex = Math.min(optionButtons.length - 1, activeIndex + 1);
          applyActive();
        }
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (optionButtons.length) {
          activeIndex = Math.max(0, activeIndex - 1);
          applyActive();
        }
      } else if (e.key === 'Enter') {
        // Toggle the highlighted option (a directory match or the typed
        // email); selecting keeps the picker open.
        e.preventDefault();
        if (optionButtons[activeIndex]) optionButtons[activeIndex].click();
      } else if (e.key === 'Escape' && options.onEscape) {
        options.onEscape(e);
      }
    });

    return {
      get mode() { return mode; },
      setMode,
      get visitors() { return lists.visitors; },
      get maintainers() { return lists.maintainers; },
      // set replaces the visitor and maintainer lists without notifying.
      set(visitors, maintainers) {
        lists.visitors = [...visitors];
        lists.maintainers = [...maintainers];
        render();
      },
      // clear empties the list on the current tab.
      clear() {
        lists[mode] = [];
        render();
      },
      // add puts an entry on a list if it is not already there.
      add(list, entry) {
        if (!lists[list].some((item) => key(item) === key(entry))) lists[list] = [...lists[list], entry];
        render();
      },
      resetSearch() {
        search.value = '';
        directory = [];
        activeIndex = 0;
      },
      focus() {
        activeIndex = 0;
        search.focus();
      },
      render,
    };
  };

  window.SpotAccess = Object.freeze({ createPicker, parsePolicy, entries, values, matches, key });
})();
