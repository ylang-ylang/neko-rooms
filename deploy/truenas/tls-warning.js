// Observe the native connection; do not create probes, change ICE or expose credentials.
(() => {
  const Native = window.RTCPeerConnection;
  if (location.protocol !== 'https:' || !Native) return;
  const options = document.currentScript ? document.currentScript.dataset : {};
  const caPath = options.caUrl === undefined ? '/ca.crt' : options.caUrl;
  const httpPath = options.httpUrl || '';
  const surface = options.surface || 'room';
  const dismissKey = 'neko-tls-ca-notice-dismissed:' + location.origin + ':' + caPath;
  const states = new Set();
  const watched = new WeakMap();
  let banner = null, dismissed = false;
  const element = (tag, text) => {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    return node;
  };
  const link = (text, href, protocol) => {
    if (!href) return null;
    const url = new URL(href, location.href);
    if (url.protocol !== protocol || url.username || url.password) return null;
    const node = element('a', text);
    node.href = url.href;
    node.style.cssText = 'display:inline-block;margin:8px 18px 0 0;color:#173e73;text-decoration:underline;font-weight:700';
    return node;
  };
  const hide = () => { if (banner) banner.remove(); banner = null; };
  const working = () => [...states].some(s => !s.closed && (s.relay || s.connected));
  const show = state => {
    if (dismissed || banner || (surface !== 'manage' && working())) return;
    banner = element('aside');
    banner.setAttribute('data-neko-tls-warning', '');
    banner.setAttribute('role', 'alert');
    banner.style.cssText = 'position:fixed;z-index:100000;top:12px;left:12px;right:12px;max-width:880px;margin:auto;padding:18px 46px 18px 20px;box-sizing:border-box;border:2px solid #b56e00;border-radius:10px;background:#fff0bd;color:#241900;box-shadow:0 8px 30px #0006;font:15px/1.5 system-ui,sans-serif;max-height:calc(100vh - 24px);overflow:auto;text-align:left';
    const close = element('button', '×');
    close.type = 'button';
    close.setAttribute('aria-label', 'Dismiss TURN/TLS warning');
    close.style.cssText = 'position:absolute;right:12px;top:8px;border:0;background:none;color:#241900;font-size:26px;cursor:pointer';
    close.addEventListener('click', () => {
      dismissed = true;
      if (surface === 'manage') { try { localStorage.setItem(dismissKey, '1'); } catch {} }
      hide();
    });
    const title = surface === 'manage'
      ? 'Private CA required for HTTPS room video (TURN/TLS)'
      : 'TURN/TLS connection failed — check certificate trust';
    banner.append(close, element('strong', title));
    banner.append(element('p', 'Continuing past an HTTPS warning does not install its CA. If you skipped that warning, your browser may still reject TURN/TLS. Import this deployment’s CA and fully restart the browser. DNS, network or server failures can also produce this error.'));
    if (state.firefoxIP) banner.append(element('p', 'Firefox also requires a DNS hostname for TURN/TLS. Importing a CA cannot fix an IP-address TURNS URL.'));
    const ca = link('Download private CA', caPath, 'https:');
    if (ca) { ca.download = 'neko-private-ca.crt'; banner.append(ca); }
    else banner.append(element('p', 'No CA download is configured. Ask this deployment’s administrator for its CA certificate.'));
    const http = link('Use the HTTP entry instead', httpPath, 'http:');
    if (http) banner.append(http);
    const instructions = element('details');
    instructions.append(element('summary', 'How to trust the CA'));
    instructions.append(element('p', 'Chromium: Settings → Privacy and security → Security → Manage certificates → Authorities / Custom roots → Import. Select the downloaded CA. Close all browser windows and fully restart Chromium.'));
    instructions.append(element('p', 'Firefox: Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import. Select the CA and enable “Trust this CA to identify websites”. Fully restart Firefox.'));
    instructions.append(element('p', 'Reload this room after restarting. If TURN/TLS still fails, check the configured hostname, port and TURN service; certificate trust is not the only possible cause.'));
    banner.append(instructions);
    (document.body || document.documentElement).append(banner);
  };
  if (surface === 'manage') {
    let alreadyDismissed = false;
    try { alreadyDismissed = localStorage.getItem(dismissKey) === '1'; } catch {}
    if (!alreadyDismissed && caPath) {
      const mount = () => show({firefoxIP: /Firefox\//.test(navigator.userAgent) && /^(?:\[|\d+\.\d+\.\d+\.\d+$)/.test(location.hostname || '')});
      if (document.readyState === 'loading' && document.addEventListener) {
        document.addEventListener('DOMContentLoaded', mount, {once: true});
      } else {
        mount();
      }
    }
    return;
  }
  // Help controls must not forward keyboard input into the remote desktop.
  for (const type of ['keydown', 'keyup', 'keypress']) document.addEventListener(type, event => {
    if (event.target.closest && event.target.closest('[data-neko-tls-warning]')) event.stopImmediatePropagation();
  }, true);
  const urls = config => (config && config.iceServers || []).flatMap(s => s.urls || s.url || []).filter(u => typeof u === 'string' && /^turns:/i.test(u));
  const observe = (pc, config) => {
    const turnURLs = urls(config);
    let state = watched.get(pc);
    if (state) {
      state.active = turnURLs.length > 0;
      if (!state.active) { clearTimeout(state.timer); state.timer = null; hide(); }
      return;
    }
    if (!turnURLs.length) return;
    state = {active:true, relay:false, connected:false, closed:false, timer:null,
      firefoxIP:/Firefox\//.test(navigator.userAgent) && turnURLs.some(u => /^turns:(?:\[|\d+\.\d+\.\d+\.\d+(?::|\?))/i.test(u))};
    states.add(state); watched.set(pc, state);
    const success = () => {
      state.connected = ['connected', 'completed'].includes(pc.iceConnectionState) || pc.connectionState === 'connected';
      state.closed = pc.connectionState === 'closed' || pc.iceConnectionState === 'closed';
      if (state.connected) { clearTimeout(state.timer); state.timer = null; hide(); }
      if (state.closed && !state.timer) states.delete(state);
    };
    pc.addEventListener('icecandidate', event => {
      const c = event.candidate;
      if (c && (c.type === 'relay' || /\btyp relay\b/.test(c.candidate || ''))) {
        state.relay = true; clearTimeout(state.timer); state.timer = null; hide();
      }
    });
    pc.addEventListener('icecandidateerror', event => {
      // 701 means unreachable, NOT “CA untrusted”. Require a TURNS failure and
      // no usable relay/connection; show conditional help, never a CA diagnosis.
      if (!state.active || !/^turns:/i.test(event.url || '') ||
          !(event.errorCode === 701 || /certificate|handshake|\bTLS\b|\bSSL\b/i.test(event.errorText || '')) ||
          state.relay || state.connected || state.timer || working()) return;
      state.timer = setTimeout(() => { state.timer = null; if (state.active) show(state); if (state.closed) states.delete(state); }, 3000);
    });
    pc.addEventListener('connectionstatechange', success);
    pc.addEventListener('iceconnectionstatechange', success);
    success();
  };
  window.RTCPeerConnection = class extends Native {
    constructor(config, ...rest) { super(config, ...rest); observe(this, config); }
    setConfiguration(config) { const result = super.setConfiguration(config); observe(this, config); return result; }
  };
})();
