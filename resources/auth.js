/* Console auth: reuses the remembered CPA / CPAMP login for this exact
 * endpoint and keeps manually entered management keys under the same rules as
 * the official panel. Mirrors the ClinePassBridge console auth module. */
(() => {
  'use strict';
  const PLUGIN = 'command-code-cpa-bridge';
  const apiMeta = document.querySelector('meta[name="cpa-api-base"]');

  // The console may be mounted behind a reverse-proxy prefix and under either
  // /v0 or /v8. Rewrite the configured base once so every request and saved
  // login carries the same prefix and version as the page itself.
  const consoleMatch = location.pathname.match(new RegExp('^(.*)/v(?:0|8)/resource/plugins/' + PLUGIN + '/console/?$'));
  const rootPrefix = consoleMatch ? consoleMatch[1] : '';
  const version = (location.pathname.match(/\/v(0|8)\/resource\//) || [])[1];
  if (apiMeta && apiMeta.content) {
    let base = apiMeta.content.trim();
    if (version) base = base.replace(/^\/v[08]\//, '/v' + version + '/');
    apiMeta.content = rootPrefix + base;
  }

  function endpoint() {
    if (!apiMeta || !apiMeta.content) return null;
    let own;
    try { own = new URL(apiMeta.content, location.origin); } catch (_) { return null; }
    if (own.origin !== location.origin || own.username || own.password || own.search || own.hash) return null;
    const full = own.pathname.replace(/\/+$/, '');
    const suffix = new RegExp('/v(?:0|8)/management/plugins/' + PLUGIN + '/?$');
    if (!suffix.test(full)) return null;
    return { origin: own.origin, path: full, root: full.replace(suffix, '') };
  }

  function sessionKey() {
    const own = endpoint();
    return own ? `${PLUGIN}:management-auth:${own.origin}${own.path}` : null;
  }

  function headersFor(login) {
    if (!login || typeof login.key !== 'string' || !login.key.trim()) return {};
    return login.type === 'x-management-key'
      ? { 'X-Management-Key': login.key.trim() }
      : { 'Authorization': 'Bearer ' + login.key.trim() };
  }

  function readManual(persistent) {
    try {
      const name = sessionKey();
      return name ? JSON.parse((persistent ? localStorage : sessionStorage).getItem(name)) : null;
    } catch (_) { return null; }
  }

  function readSaved() {
    try {
      if (localStorage.getItem('isLoggedIn') !== 'true') return null;
      let value = localStorage.getItem('cli-proxy-auth');
      if (!value) return null;
      const v2 = value.startsWith('enc::v2::');
      if (v2 || value.startsWith('enc::v1::')) {
        const bytes = Uint8Array.from(atob(value.slice(9)), (c) => c.charCodeAt(0));
        const mask = new TextEncoder().encode(v2
          ? `cli-proxy-api-webui::secure-storage|v2|${location.host}`
          : `cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`);
        value = new TextDecoder().decode(bytes.map((byte, i) => byte ^ mask[i % mask.length]));
      }
      const saved = JSON.parse(value)?.state;
      if (!saved || saved.rememberPassword !== true || typeof saved.managementKey !== 'string' || !saved.managementKey.trim()) return null;
      const server = new URL(saved.apiBase);
      const own = endpoint();
      const cleanPath = (path) => path.replace(/\/+$/, '').replace(/\/v(?:0|8)\/management$/, '');
      if (!own || server.username || server.password || server.search || server.hash || server.origin !== own.origin) return null;
      if (cleanPath(server.pathname) !== own.root) return null;
      return { key: saved.managementKey.trim(), type: 'bearer' };
    } catch (_) { return null; }
  }

  window.CCBridgeAuthHeaders = () => {
    for (const login of [readManual(false), readManual(true), readSaved()]) {
      const headers = headersFor(login);
      if (Object.keys(headers).length) return headers;
    }
    return {};
  };

  window.CCBridgeRememberAuth = () => Object.keys(headersFor(readManual(true))).length > 0;

  window.CCBridgeSaveAuth = (key, type, remember = false) => {
    const name = sessionKey();
    if (!name || typeof key !== 'string' || !key.trim()) return;
    const value = JSON.stringify({ key: key.trim(), type: type === 'x-management-key' ? type : 'bearer' });
    try { sessionStorage.setItem(name, value); } catch (_) { /* the page still holds the key */ }
    try {
      if (remember) localStorage.setItem(name, value);
      else localStorage.removeItem(name);
    } catch (_) { /* one blocked store must not prevent using the other */ }
  };

  window.CCBridgeClearAuth = (rejectedHeaders) => {
    const name = sessionKey();
    if (!name) return;
    for (const persistent of [false, true]) {
      try {
        const saved = headersFor(readManual(persistent));
        if (!rejectedHeaders || Object.keys(saved).some((header) => saved[header] === rejectedHeaders[header])) {
          (persistent ? localStorage : sessionStorage).removeItem(name);
        }
      } catch (_) { /* clearing a blocked store must not break the page */ }
    }
  };
})();
