// siteinfo debug page. Plain Vue 3 (vendored global build), no build step.
const { createApp, ref, reactive, onMounted, onBeforeUnmount } = Vue;

const ROUTES = ['start', 'crawler', 'debug'];
const EXAMPLES = ['tagesschau.de', 'orf.at', 'srf.ch', 'fritz-kola.com'];

const HINTS = {
  invalid_url: 'Die Eingabe wurde von der URL-Normalisierung abgelehnt (kein http/https, IP-Adresse, localhost, Port, Userinfo, Fragment …). Siehe den Trace.',
  imprint_not_found: 'Auf der Startseite wurde kein Impressum-Link gefunden und die Standardpfade (/impressum, /imprint, /legal-notice) lieferten keine Impressum-Seite. Das Ergebnis wird 24 h negativ gecacht. Der Trace zeigt die bewerteten Link-Kandidaten und die Ablehnungsgründe.',
  upstream_error: 'Firecrawl konnte die Seite nicht abrufen (z. B. DNS-Fehler, Seite antwortet mit Fehlerstatus, Blockade). Ein Fehlschlag wird 5 Minuten negativ gecacht; „Cache leeren" hebt das auf.',
  timeout: 'Firecrawl oder die Anfrage hat das Zeitlimit überschritten (Firecrawl 30 s je Aufruf, Load 40 s, Handler 45 s). Wird nicht gecacht.',
  upstream_unavailable: 'Firecrawl ist gestört (5xx, nicht erreichbar) oder lehnt das Konto ab (API-Key, Credits, Bedingungen). Kein Problem der Website; wird nicht gecacht. Siehe Server-Log.',
  upstream_rate_limited: 'Firecrawl hat mit 429 geantwortet (Konto-Limit). Wird nicht gecacht; später erneut versuchen.',
  rate_limited: 'Pro-IP-Limit der API erreicht (protect.ip_rate). Retry-After beachten.',
  token_rate_limited: 'Pro-Token-Limit der API erreicht (protect.token_rate).',
  too_many_auth_failures: 'Zu viele fehlgeschlagene Anmeldungen von dieser IP. Prüfe, ob API_TOKEN des debug-ui-Containers zu SITEINFO_AUTH_TOKENS passt.',
  overloaded: 'Lastschutz: zu viele gleichzeitige Requests oder Upstream-Loads in diesem Pod (protect.max_*).',
  unavailable: 'Lock-Timeout, Redis nicht erreichbar oder der Dienst fährt herunter.',
  unauthorized: 'Der Proxy sendet einen Token, den die API ablehnt. API_TOKEN (debug-ui) muss in SITEINFO_AUTH_TOKENS stehen.',
  internal_error: 'Unerwarteter Fehler; Details stehen im Server-Log (Request-ID suchen).',
  'Netzwerkfehler': 'Die API ist nicht erreichbar oder die Anfrage wurde abgebrochen. Läuft der siteinfo-Container?',
};

createApp({
  setup() {
    const store = (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } };
    const save = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* private mode */ } };

    const routeFromHash = () => { const r = location.hash.replace(/^#\/?/, ''); return ROUTES.includes(r) ? r : 'start'; };
    const route = ref(routeFromHash());
    const onHash = () => { route.value = routeFromHash(); };
    const goto = (r) => { location.hash = `#/${r}`; };
    const url = ref('');
    const apiBase = ref(store('apiBase', 'http://localhost:8080'));
    const flushFirst = ref(store('flushFirst', false));
    const busy = ref(false);
    const results = ref([]);
    const history = ref(store('history', []));
    const health = reactive({ label: 'prüfe …', cls: '', detail: '' });
    const showConfig = ref(false);
    const configData = ref(null);
    const configError = ref('');
    const toastMsg = ref('');
    let nextId = 1, healthTimer = null, toastTimer = null;

    async function call(method, path) {
      const t0 = performance.now();
      try {
        const r = await fetch(path, { method, cache: 'no-store' });
        const text = await r.text();
        let json = null;
        try { json = JSON.parse(text); } catch { /* not JSON */ }
        return { ok: r.ok, status: r.status, headers: Object.fromEntries(r.headers.entries()), json, text, ms: performance.now() - t0 };
      } catch (e) {
        return { ok: false, status: 0, headers: {}, json: null, text: '', error: String(e), ms: performance.now() - t0 };
      }
    }

    function toast(msg) {
      toastMsg.value = msg;
      clearTimeout(toastTimer);
      toastTimer = setTimeout(() => { toastMsg.value = ''; }, 3500);
    }

    const enc = (s) => encodeURIComponent(s);

    async function flush() {
      const res = await call('DELETE', `/api/v1/debug/cache?url=${enc(url.value)}`);
      if (res.ok) toast(`Cache geleert (${res.json.deleted_keys} Schlüssel für ${res.json.host})`);
      else toast(`Cache leeren fehlgeschlagen: ${res.json?.error?.message || res.error || res.status}`);
      return res.ok;
    }

    async function run(kinds) {
      if (!url.value) return;
      busy.value = true;
      const input = url.value;
      history.value = [input, ...history.value.filter((h) => h !== input)].slice(0, 12);
      save('history', history.value);
      save('flushFirst', flushFirst.value);
      if (flushFirst.value) await flush();

      const runs = kinds.map(async (kind) => {
        const r = reactive({ id: nextId++, kind, input, running: true, res: null, tab: 'result' });
        results.value.unshift(r);
        r.res = await call('GET', `/api/v1/${kind}?url=${enc(input)}&debug=1`);
        r.running = false;
        r.tab = r.res.json && r.res.json._debug ? 'trace' : 'json';
      });
      await Promise.all(runs);
      results.value = results.value.slice(0, 20);
      busy.value = false;
    }

    async function checkHealth() {
      const res = await call('GET', '/api/readyz');
      if (res.status === 200) Object.assign(health, { label: 'API bereit', cls: 'ok', detail: 'Redis erreichbar' });
      else if (res.status === 503) Object.assign(health, { label: 'API nicht bereit', cls: 'warn', detail: res.json?.status || '' });
      else Object.assign(health, { label: 'API nicht erreichbar', cls: 'err', detail: res.error || `HTTP ${res.status}` });
    }

    async function toggleConfig() {
      showConfig.value = !showConfig.value;
      if (showConfig.value) await loadConfig();
    }

    async function loadConfig() {
      configData.value = null; configError.value = '';
      const res = await call('GET', '/api/v1/debug/config');
      if (res.ok) configData.value = res.json;
      else configError.value = res.json?.error?.message || `HTTP ${res.status} – ist SITEINFO_DEBUG_ENABLED gesetzt?`;
    }

    onMounted(() => {
      window.addEventListener('hashchange', onHash);
      checkHealth(); loadConfig();
      healthTimer = setInterval(checkHealth, 10000);
    });
    onBeforeUnmount(() => { clearInterval(healthTimer); window.removeEventListener('hashchange', onHash); });

    function startLookup(kinds) { goto('crawler'); run(kinds); }
    const uptime = (s) => dur(s);

    // ---- view helpers -------------------------------------------------------------------
    const dbg = (r) => (r.res && r.res.json && r.res.json._debug) || null;
    const errCode = (r) => (r.res.error ? 'Netzwerkfehler' : r.res.json?.error?.code || `HTTP ${r.res.status}`);
    const errMessage = (r) => (r.res.error ? r.res.error : r.res.json?.error?.message || r.res.text.slice(0, 200));
    const hint = (code) => HINTS[code] || '';
    const statusClass = (s) => (s >= 200 && s < 300 ? 'ok' : s >= 500 || s === 0 ? 'err' : 'warn');
    const ms = (v) => (v == null ? '' : v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${v.toFixed(0)} ms`);
    const isObject = (v) => v && typeof v === 'object' && !Array.isArray(v);

    function age(iso) {
      const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
      return dur(s);
    }
    function dur(s) {
      if (s == null || s < 0) return '—';
      if (s < 90) return `${Math.round(s)} s`;
      if (s < 5400) return `${Math.round(s / 60)} min`;
      if (s < 129600) return `${(s / 3600).toFixed(1)} h`;
      return `${(s / 86400).toFixed(1)} Tage`;
    }

    function tabsFor(r) {
      const t = [];
      if (dbg(r)) t.push('trace');
      if (dbg(r) && (dbg(r).cache || dbg(r).cache_error)) t.push('cache');
      t.push('headers', 'json', 'curl');
      return t;
    }
    function tabLabel(t, r) {
      return { result: 'Ergebnis', trace: `Trace (${dbg(r)?.trace.length ?? 0})`, cache: 'Cache', headers: 'Header', json: 'JSON', curl: 'curl' }[t];
    }

    const dash = (v) => (v === undefined || v === null || v === '' ? null : v);
    function imprintRows(j) {
      const a = j.address, c = j.contact, g = j.register;
      const addr = a ? [a.street, [a.postal_code, a.city].filter(Boolean).join(' '), a.country].filter(Boolean).join(', ') : null;
      const reg = g ? [g.type, g.number, g.court && `(${g.court})`].filter(Boolean).join(' ') : null;
      return [
        ['Firma', dash(j.company_name)], ['Rechtsform', dash(j.legal_form)],
        ['Vertreten durch', dash((j.represented_by || []).join(', '))], ['Adresse', dash(addr)],
        ['E-Mail', dash(c?.email)], ['Telefon', dash(c?.phone)], ['Fax', dash(c?.fax)],
        ['Register', dash(reg)], ['USt-IdNr.', dash(j.vat_id)], ['Aufsichtsbehörde', dash(j.supervisory_authority)],
        ['Inhaltlich verantwortlich', dash(j.responsible_for_content)], ['Land', dash(j.country)],
      ];
    }
    const brandImages = (j) => [['Logo', j.logo], ['Favicon', j.favicon], ['OG-Bild', j.og_image]].filter((x) => x[1]);
    const brandColors = (j) => Object.entries(j.colors || {}).filter((x) => x[1]);
    function brandRows(j) {
      const fonts = (j.fonts || []).map((f) => (f.role ? `${f.family} (${f.role})` : f.family)).join(', ');
      const sizes = Object.entries(j.typography?.font_sizes || {}).map(([k, v]) => `${k}: ${v}`).join(', ');
      return [
        ['Markenname', dash(j.brand_name)], ['Farbschema', dash(j.color_scheme)], ['Beschreibung', dash(j.description)],
        ['Sprache', dash(j.language)], ['Schriften', dash(fonts)], ['Schriftgrößen', dash(sizes)],
      ];
    }
    function cacheRows(i) {
      return [
        ['Schlüssel', i.cache_key], ['Lock-Schlüssel', i.lock_key], ['Eintrag vorhanden', i.exists ? 'ja' : 'nein'],
        ['Status', i.status ? `${i.status}${i.error_code ? ` (${i.error_code})` : ''}` : '—'],
        ['Alter', i.exists ? dur(i.age_seconds) : '—'], ['Restlaufzeit (TTL)', i.ttl_seconds >= 0 ? dur(i.ttl_seconds) : '—'],
        ['Größe', i.size_bytes ? `${i.size_bytes} Bytes` : '—'],
        ['Unvollständig (kurze TTL)', i.partial ? 'ja' : 'nein'], ['Lock aktuell gehalten', i.lock_held ? 'ja' : 'nein'],
        ['Refresh-Cooldown', i.cooldown_ttl_seconds >= 0 ? `noch ${dur(i.cooldown_ttl_seconds)}` : 'kein'],
      ];
    }

    const short = (v) => {
      if (v === null || v === undefined) return 'null';
      if (typeof v === 'object') return Array.isArray(v) ? `[${v.length}]` : '{…}';
      const s = String(v);
      return s.length > 80 ? `${s.slice(0, 80)}…` : s;
    };
    const bigFields = (f) => Object.entries(f).filter(([, v]) => (v && typeof v === 'object') || (typeof v === 'string' && v.length > 80));
    const pretty = (o) => JSON.stringify(o, null, 2);
    function prettyBody(res) {
      if (!res.json) return res.text || res.error || '';
      const copy = { ...res.json };
      if (typeof copy.text === 'string') copy.text = `(${copy.text.length} Zeichen, hier ausgeblendet)`;
      return pretty(copy);
    }
    function curl(r) {
      return `curl -s -H "Authorization: Bearer $TOKEN" '${apiBase.value}/v1/${r.kind}?url=${enc(r.input)}&debug=1' | jq`;
    }
    function flatten(o, prefix = '') {
      const out = {};
      for (const [k, v] of Object.entries(o)) {
        if (isObject(v)) Object.assign(out, flatten(v, `${prefix}${k}.`));
        else out[`${prefix}${k}`] = Array.isArray(v) ? v.join(', ') || '—' : v === '' ? '—' : v;
      }
      return out;
    }
    async function copy(text) {
      try { await navigator.clipboard.writeText(text); toast('In die Zwischenablage kopiert'); }
      catch { toast('Kopieren nicht möglich (HTTPS/Localhost nötig)'); }
    }
    const watchApi = () => save('apiBase', apiBase.value);

    return {
      route, goto, startLookup, uptime, examples: EXAMPLES, url, apiBase, flushFirst, busy, results, history, health, showConfig, configData, configError, toastMsg,
      run, flush, toggleConfig, copy, dbg, errCode, errMessage, hint, statusClass, ms, age, isObject, flatten,
      tabsFor, tabLabel, imprintRows, brandImages, brandColors, brandRows, cacheRows, short, bigFields, pretty,
      prettyBody, curl, watchApi,
    };
  },
}).mount('#app');
