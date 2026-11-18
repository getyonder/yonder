// The window's side of Yonder: it draws the State that Go sends and passes
// clicks back. Names and file names come from other devices, so everything
// is put on the page as text, never as HTML.
import { go, on, copy, web } from './bridge.js';

const $ = id => document.getElementById(id);

// What only the window knows: which page and dialog are open.
const ui = { tab: 'devices', menu: null, unpairing: false, pair: null, pairStarted: false, joining: false, coding: false, asking: null, label: null };
let state = null;

// h builds an element: h('button', {class: 'btn', onclick: f}, 'Send').
function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, '');
    else if (v !== false && v != null) el.setAttribute(k, v);
  }
  el.append(...children.filter(c => c != null && c !== false));
  return el;
}

const plural = (n, noun) => n === 1 ? `1 ${noun}` : `${n} ${noun}s`;

function size(bytes) {
  const units = ['bytes', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, n = bytes;
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
  return i === 0 ? plural(bytes, 'byte') : `${n < 100 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

// progressText writes "29.9 of 48.2 MB" with one unit for both numbers.
function progressText(done, total) {
  const whole = size(total), unit = whole.split(' ')[1];
  if (unit === 'bytes' || unit === 'byte') return `${done} of ${whole}`;
  const scale = 1000 ** ['KB', 'MB', 'GB', 'TB'].indexOf(unit) * 1000;
  const part = done / scale;
  return `${part < 100 ? part.toFixed(1) : Math.round(part)} of ${whole}`;
}

function timeLeft(seconds) {
  if (seconds < 60) return `about ${Math.max(1, Math.round(seconds))} s left`;
  if (seconds < 3600) return `about ${Math.round(seconds / 60)} min left`;
  return `about ${Math.round(seconds / 3600)} h left`;
}

function when(iso) {
  const at = new Date(iso), now = new Date();
  const clock = at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return at.toDateString() === now.toDateString() ? `today ${clock}` : `${at.toLocaleDateString()} ${clock}`;
}

let toastTimer;
function say(message) {
  const t = $('toast');
  t.textContent = message;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 6000);
}
// tell shows the reason when something asked of Go fails.
const tell = err => say(err?.message || String(err));

// drawIfChanged redraws one part of the window only when what it shows has
// changed, so progress ticks do not disturb the rest.
const drawn = {};
function drawIfChanged(part, data, draw) {
  const key = JSON.stringify(data);
  if (drawn[part] === key) return;
  drawn[part] = key;
  draw();
}

function render() {
  if (!state) return;
  for (const tab of ['devices', 'received', 'settings']) $('tab-' + tab).hidden = tab !== ui.tab;
  for (const b of $('nav').children) b.dataset.tab === ui.tab ? b.setAttribute('aria-current', 'page') : b.removeAttribute('aria-current');

  drawIfChanged('me', [state.name, state.problem, state.unread], () => {
    $('me-name').textContent = state.name;
    $('version').textContent = state.version;
    $('me-status').textContent = state.problem || 'Ready to receive';
    $('me-status').className = state.problem ? 'problem' : '';
    $('unread').textContent = state.unread;
    $('unread').hidden = state.unread === 0;
  });
  drawIfChanged('blocked', state.blocked, () => {
    const b = state.blocked;
    $('blocked').hidden = !b;
    if (!b) return;
    $('blocked-title').textContent = b.title;
    $('blocked-text').textContent = b.text;
    $('blocked-fix').textContent = b.button;
    $('blocked-fix').hidden = !b.button;
  });
  drawIfChanged('withdrawn', state.withdrawn, () => {
    const w = state.withdrawn;
    $('withdrawn').hidden = !w;
    if (!w) return;
    $('withdrawn-title').textContent = `Yonder ${w.have} has been withdrawn`;
    $('withdrawn-text').textContent = w.version
      ? `Get Yonder ${w.version} in its place. Your paired devices and settings stay.`
      : 'A new version is not out yet. Until it is, use this one at your own risk.';
    $('withdrawn-get').textContent = `Get Yonder ${w.version}`;
    $('withdrawn-get').parentElement.hidden = !w.version;
  });
  drawIfChanged('update', state.update, () => {
    const u = state.update;
    $('update').hidden = !u;
    if (!u) return;
    $('update-title').textContent = `Yonder ${u.version} is out`;
    $('update-text').textContent = `You have ${u.have}. Download the new version and install it over this one.`;
  });
  drawIfChanged('started', state.started, () => {
    const n = state.started;
    $('started').hidden = !n;
    if (!n) return;
    $('started-title').textContent = `Yonder ${n.version} was started, but this older one is still running`;
    $('started-text').textContent = `This window is Yonder ${n.have}. Only one Yonder can run at a time, so the new one closed. Quit this one, then start Yonder again.`;
  });
  drawIfChanged('refresh', !!state.refreshing, () => {
    const b = $('refresh'), label = state.refreshing ? 'Refreshing…' : 'Refresh the list';
    b.disabled = state.refreshing;
    b.classList.toggle('busy', !!state.refreshing);
    b.title = label;
    b.setAttribute('aria-label', label);
  });
  drawIfChanged('devices', [state.devices, state.visible, ui.menu, ui.unpairing, ui.asking], drawDevices);
  drawIfChanged('transfers', state.transfers, drawTransfers);
  drawIfChanged('received', [state.received, state.saveDir, state.folder], drawReceived);
  drawIfChanged('settings', [state.name, state.saveDir, state.folder, state.autostart, state.visible, state.updateCheck], () => {
    $('set-updates').checked = !!state.updateCheck;
    $('set-autostart').checked = state.autostart;
    $('set-visible').checked = state.visible;
    if (document.activeElement !== $('set-name')) $('set-name').value = state.name;
    // A web page has a folder only where the browser can give it one, and
    // then learns the folder's name, never its path (state.folder).
    $('field-folder').hidden = web && !state.folder;
    $('set-folder').value = web ? state.folder?.name ?? '' : state.saveDir;
    $('stop-folder').hidden = !state.folder?.name;
    $('folder-hint').hidden = !web;
    $('folder-hint').textContent = state.folder?.name
      ? 'Each file is saved in this folder when it arrives.'
      : 'Choose a folder and Yonder saves each file there when it arrives. Without one, files wait in this page until you press Save.';
  });
  drawDialogs();
}

function deviceDetail(d) {
  if (d.nearby) return d.paired ? 'Online, same network' : [d.model, 'same network'].filter(Boolean).join(', ').replace(/^s/, 'S');
  if (d.presence === 'online') return 'Online, on another network';
  if (d.presence === 'checking') return 'Checking whether it is online…';
  return 'Offline, or it has unpaired this device';
}

function drawDevices() {
  const rows = state.devices.map(d => {
    const offline = d.presence === 'offline';
    const menuOpen = ui.menu === d.id;
    return h('div', { class: 'row', id: 'dev-' + d.id, 'data-file-drop-target': !offline && !d.pairable },
      h('span', { class: 'dot' + (offline ? ' off' : d.presence === 'checking' ? ' checking' : '') }),
      h('div', null,
        h('div', { class: 'name' }, d.name, d.label && h('span', { class: 'own' }, d.label)),
        h('div', { class: 'sub' },
          d.paired && h('span', { class: 'tag paired' }, 'Paired'),
          d.nearby && h('span', { class: 'tag' }, 'Nearby'),
          d.localsend && h('span', { class: 'tag' }, 'LocalSend'),
          d.unencrypted && h('span', { class: 'tag warn' }, 'Not encrypted'),
          h('span', null, deviceDetail(d)))),
      h('div', { class: 'acts' },
        d.pairable
          ? h('button', { class: 'btn primary', disabled: ui.asking === d.id, onclick: () => pairNearby(d) }, ui.asking === d.id ? 'Asking…' : 'Pair')
          : h('button', { class: offline ? 'btn' : 'btn primary', disabled: offline, onclick: () => go('PickAndSend', d.id).catch(tell) }, 'Send files'),
        d.paired && h('button', { class: 'btn quiet', 'aria-label': 'More for ' + d.name, 'aria-expanded': String(menuOpen), onclick: e => { e.stopPropagation(); ui.menu = menuOpen ? null : d.id; ui.unpairing = false; render(); } }, '⋯')),
      menuOpen && h('div', { class: 'menu', onclick: e => e.stopPropagation() },
        h('button', { onclick: () => openLabel(d) }, 'Give it your own name…'),
        h('button', { onclick: () => { ui.menu = null; go('SetAlways', d.id, !d.always).catch(tell); } }, d.always ? '✓ Always accept files from this device' : 'Always accept files from this device'),
        h('button', { class: 'danger', onclick: () => {
          if (!ui.unpairing) { ui.unpairing = true; render(); return; }
          ui.menu = null; ui.unpairing = false;
          go('Unpair', d.id).then(() => say(`Unpaired ${d.name}.`), tell);
        } }, ui.unpairing ? `Unpair ${d.name}? Click again to confirm` : 'Unpair')));
  });
  if (rows.length === 0) {
    rows.push(h('p', { class: 'empty' }, web
      ? state.visible
        ? 'No devices yet. Devices on this network with Yonder open appear here by themselves. To reach a device on another network, pair it.'
        : 'No devices yet. Pair a device to send it files and receive files from it.'
      : 'No devices yet. Devices on this network running Yonder or LocalSend appear here by themselves. To reach a device on another network, pair it.'));
  }
  $('devices').replaceChildren(...rows);
}

function drawTransfers() {
  $('drophint').hidden = state.transfers.length > 0 || state.devices.length === 0;
  $('transfers').replaceChildren(...state.transfers.map(t => {
    const files = plural(t.files, 'file');
    const cancel = h('button', { class: 'btn', onclick: () => go('CancelTransfer', t.id) }, 'Cancel');
    // A web page stops when its tab is closed or a phone's screen locks.
    const stay = web && h('div', { class: 'hint' }, 'Keep this page open until this is done. On a phone, keep the screen on too.');
    if (t.phase === 'done' || t.phase === 'failed') {
      return h('div', { class: 'dock ' + t.phase },
        h('div', { class: 'top' }, h('b', null, t.note),
          h('span', { class: 'acts' },
            // A web page holds what it received until it is saved (t.unsaved,
            // from its bridge): the files can be saved from here too.
            t.unsaved > 0 && h('button', { class: 'btn primary', onclick: () => go('SaveTransfer', t.id).catch(tell) }, 'Save'),
            t.unsaved === 0 && h('span', { class: 'hint' }, 'Saved'),
            h('button', { class: 'btn', onclick: () => go('DismissTransfer', t.id) }, 'Dismiss'))));
    }
    if (t.phase === 'waiting') {
      const title = !t.sending ? `Receiving ${files} from ${t.device}`
        : t.connecting ? `Connecting to ${t.device}…` : `Waiting for ${t.device} to accept ${files}`;
      return h('div', { class: 'dock' },
        h('div', { class: 'top' }, h('b', null, title), cancel),
        h('div', { class: 'bar waiting' }, h('i')), stay);
    }
    const title = t.sending ? `Sending ${files} to ${t.device}` : `Receiving ${files} from ${t.device}`;
    const fill = h('i');
    fill.style.width = (t.total > 0 ? 100 * t.done / t.total : 100) + '%';
    // A send's count is what has left this program, and the last of it can
    // still be on its way. The send is done when the other device says so.
    const leaving = t.sending && t.total > 0 && t.done >= t.total;
    return h('div', { class: 'dock' },
      h('div', { class: 'top' }, h('b', null, title),
        h('span', { class: 'acts' }, t.relayed && h('span', { class: 'chip', title: 'The two networks would not connect directly, so this is going through a relay.' }, 'Relayed, slower'),
          t.direct && h('span', { class: 'chip direct', title: 'This is going straight between the two devices, not through a relay.' }, 'Direct'), cancel)),
      leaving ? h('div', { class: 'bar waiting' }, h('i')) : h('div', { class: 'bar' }, fill),
      leaving
        ? h('div', { class: 'nums' }, h('span', null, `Waiting for ${t.device} to receive the last of it…`))
        : h('div', { class: 'nums' },
          h('span', null, t.file),
          h('span', null, progressText(t.done, t.total)),
          t.rate > 0 && h("span", null, size(Math.round(t.rate)) + "/s"),
          t.rate > 0 && t.done < t.total && h('span', null, timeLeft((t.total - t.done) / t.rate))),
      stay);
  }));
}

function drawReceived() {
  $('received').replaceChildren(...state.received.map((f, i) =>
    h('div', { class: 'row' }, h('span'),
      h('div', null,
        h('div', { class: 'name' }, f.name),
        h('div', { class: 'sub' }, f.from && h('span', null, 'From ' + f.from), h('span', null, when(f.at)), h('span', null, size(f.size)))),
      h('div', { class: 'acts' }, f.kept
        ? h('span', { class: 'hint' }, 'Saved')
        : h('button', { class: 'btn', onclick: () => go('OpenReceived', i).catch(tell) }, web ? 'Save' : f.runs ? 'Show' : 'Open')))));
  // A web page holds what arrives until it is saved: by itself into the
  // folder the person chose, where the browser has one to give and still
  // allows it, and by the Save button if not.
  const folder = web && state.folder;
  $('folder-ask').hidden = !folder?.ask;
  $('folder-tip').hidden = !folder || !!folder.name || !state.received.some(f => !f.kept);
  if (folder?.ask) $('folder-ask-title').textContent = `Yonder needs your yes to save into "${folder.name}" again`;
  $('received-hint').textContent = web
    ? state.received.some(f => !f.kept)
      ? 'These are kept only while this page is open. Save the ones you want.'
      : folder?.saving
        ? (state.received.length ? `Saved in the folder "${folder.name}".` : `Nothing received since this page was opened. Files are saved in the folder "${folder.name}".`)
        : 'Nothing received since this page was opened.'
    : state.received.length
      ? `Saved in ${state.saveDir}.`
      : `Nothing received since Yonder started. Files are saved in ${state.saveDir}.`;
}

// pairNearby asks a device on the same network to pair; when it answers,
// the State brings the code for both owners to compare.
function pairNearby(d) {
  if (ui.asking) return;
  ui.asking = d.id;
  render();
  go('PairNearby', d.id).catch(tell).then(() => { ui.asking = null; render(); });
}

// One dialog at a time: a question from another device comes before the
// pairing dialog the person opened themselves.
function drawDialogs() {
  const offer = state.offer, pairing = state.pairing;
  // The pairing finished or was closed from the Go side.
  if (ui.pair === 'show' && ui.pairStarted && !pairing) { ui.pair = null; ui.pairStarted = false; }
  // The two devices have met by a code, or a nearby device asks: the dialog
  // with the code to compare takes over from the tab.
  if (ui.pair === 'code' && state.code) {
    if (state.room) go('StopCode');
    ui.pair = null;
  }
  const confirm = !offer && pairing?.request;

  $('ov-offer').hidden = !offer;
  drawIfChanged('offer', offer, () => {
    if (!offer) return;
    $('in-h').textContent = `${offer.from} wants to send you ${plural(offer.files.length, 'file')}`;
    $('in-files').replaceChildren(...offer.files.map(f => h('div', null, h('span', null, f.name), h('span', null, size(f.size)))));
    $('in-unpaired').hidden = offer.paired;
    $('in-always-row').hidden = !offer.paired;
    $('in-always').checked = false;
    $('in-always-text').textContent = `Always accept from ${offer.from}`;
  });

  $('ov-confirm').hidden = !confirm;
  if (confirm) $('cf-h').textContent = `Pair with ${pairing.request}?`;

  const code = !offer && !confirm && state.code;
  $('ov-code').hidden = !code;
  drawIfChanged('code', code, () => {
    if (!code) return;
    $('code-h').textContent = code.asked ? `${code.name} wants to pair` : `Pair with ${code.name}?`;
    $('code-digits').textContent = code.code;
    $('code-text').textContent = `Check that ${code.name} shows the same code. If it shows another, don't pair: it is a different device, or something is in between.`;
    $('code-wait').hidden = !code.waiting;
    $('code-wait-text').textContent = `Waiting for ${code.name}…`;
    $('code-yes').hidden = code.waiting;
    $('code-no').textContent = code.waiting ? 'Cancel' : 'Don\'t pair';
  });

  // The device was unpaired while its name was being typed.
  if (ui.label && !state.devices.some(d => d.id === ui.label && d.paired)) ui.label = null;
  const label = !offer && !confirm && !code && ui.label;
  $('ov-label').hidden = !label;

  $('ov-pair').hidden = !ui.pair || !!offer || !!confirm || !!code;
  focusDialog(offer ? 'ov-offer' : confirm ? 'ov-confirm' : code ? 'ov-code' : label ? 'ov-label' : ui.pair ? 'ov-pair' : null);
  if (!ui.pair) return;
  for (const tab of ['show', 'have', 'code']) {
    $('pp-' + tab).hidden = ui.pair !== tab;
    $('pt-' + tab).setAttribute('aria-selected', ui.pair === tab);
  }
  drawIfChanged('pairing', pairing, () => {
    const link = pairing?.link, problem = pairing?.problem;
    $('qr').hidden = !pairing?.qr;
    if (pairing?.qr) $('qr').replaceChildren(qrSVG(pairing.qr));
    $('pair-text').textContent = link ? 'Scan this with the other device’s camera, or send it the link. Opening it starts Yonder in a browser; in the app, paste it under “I have a link”.' : '';
    $('pair-text').hidden = !link;
    $('pair-link-text').textContent = link || '';
    $('pair-link-text').hidden = !link;
    $('pair-copy').disabled = !link;
    $('pair-copy').textContent = 'Copy link';
    $('pair-wait').className = problem ? 'wait problem' : 'wait';
    if (problem || !link) $('pair-wait-text').textContent = problem || 'Making a link…';
    else drawLinkLife();
  });
  $('join').disabled = ui.joining;
  $('join').textContent = ui.joining ? 'Pairing…' : 'Pair';
  drawIfChanged('room', state.room, () => {
    // With a problem the tab is as before a code was asked for, and says why.
    const room = state.room, code = room?.code, problem = room?.problem;
    const showing = !!room && !problem;
    $('room-start').hidden = showing;
    $('room-digits').textContent = code ? code.slice(0, 3) + ' ' + code.slice(3) : '';
    $('room-digits').hidden = !code;
    $('room-text').hidden = !code;
    $('room-wait').hidden = !room;
    $('room-wait').className = problem ? 'wait problem' : 'wait';
    $('room-wait-text').textContent = problem || (code ? 'Waiting for the other device.' : 'Getting a code…');
    $('room-life').hidden = !code;
    $('room-field').hidden = showing;
    $('room-pair').hidden = showing;
    if (showing) $('room-note').hidden = true;
  });
  $('room-pair').disabled = ui.coding;
  $('room-pair').textContent = ui.coding ? 'Pairing…' : 'Pair';
}

// drawLinkLife says how long the link shown works, with the time left. A
// new link looks much like the last one, so this is the only sign that an
// old one has stopped.
function drawLinkLife() {
  const p = state?.pairing;
  if (!p?.link || p.problem) return;
  const left = Math.max(0, (Date.parse(p.expires) - Date.now()) / 1000);
  $('pair-wait-text').replaceChildren('Waiting for the other device. The link works once, for 10 minutes, and only while this stays open: ',
    h('b', null, left < 60 ? 'less than a minute left.' : `${plural(Math.ceil(left / 60), 'minute')} left.`));
}
setInterval(() => { if (ui.pair === 'show') drawLinkLife(); }, 5000);

// focusDialog keeps the keyboard inside the open dialog: the rest of the
// window stops taking keys, and focus moves to the dialog itself, not to a
// button, so a stray Enter answers nothing.
let openDialog = null;
function focusDialog(id) {
  if (id === openDialog) return;
  openDialog = id;
  for (const el of document.querySelectorAll('.side, .main')) el.inert = !!id;
  if (id) $(id).querySelector('.dialog').focus();
}

function qrSVG(rows) {
  const ns = 'http://www.w3.org/2000/svg', n = rows.length, quiet = 2;
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', `${-quiet} ${-quiet} ${n + 2 * quiet} ${n + 2 * quiet}`);
  svg.setAttribute('shape-rendering', 'crispEdges');
  let d = '';
  rows.forEach((row, y) => { for (let x = 0; x < n; x++) if (row[x] === '1') d += `M${x} ${y}h1v1h-1z`; });
  const path = document.createElementNS(ns, 'path');
  path.setAttribute('d', d);
  path.setAttribute('fill', '#000000');
  svg.append(path);
  return svg;
}

// openLabel asks for the owner's own name for a paired device.
function openLabel(d) {
  ui.menu = null;
  ui.label = d.id;
  $('label-h').textContent = `Your name for ${d.name}`;
  $('label-name').value = d.label;
  render();
  setTimeout(() => $('label-name').select());
}

function closeLabel() {
  ui.label = null;
  render();
}

function saveLabel() {
  go('SetLabel', ui.label, $('label-name').value).then(closeLabel, tell);
}

function openPair(tab) {
  if (ui.pair === tab) return;
  leavePairTab();
  ui.pair = tab;
  ui.pairStarted = false;
  // Only once the State shows the pairing can its going away mean it is over.
  if (tab === 'show') go('StartPairing').then(() => go('State')).then(s => { take(s); ui.pairStarted = !!state.pairing; render(); }, tell);
  // The box starts empty each time: a link that failed once is used up.
  if (tab === 'have') { $('pair-link').value = ''; $('join-wait').hidden = true; setTimeout(() => $('pair-link').focus()); }
  if (tab === 'code') $('room-note').hidden = true;
  render();
}

// leavePairTab stops what the open tab of the pairing dialog had going: a
// link or a code that this device shows.
function leavePairTab() {
  if (ui.pair === 'show') go('StopPairing');
  if (ui.pair === 'code' && state?.room) go('StopCode');
}

function closePair() {
  leavePairTab();
  ui.pair = null;
  ui.pairStarted = false;
  render();
}

function join() {
  const link = $('pair-link').value.trim();
  if (!link || ui.joining) return;
  ui.joining = true;
  $('join-wait').className = 'wait';
  $('join-wait-text').textContent = 'Waiting for the other device to say yes…';
  $('join-wait').hidden = false;
  render();
  go('Join', link).then(name => {
    ui.joining = false;
    $('pair-link').value = '';
    say(`Paired with ${name}.`);
    closePair();
  }, err => {
    ui.joining = false;
    $('join-wait').className = 'wait problem';
    $('join-wait-text').textContent = err?.message || String(err);
    render();
  });
}

// showRoomCode has this device show a code to type on the other one; the
// State brings the code.
function showRoomCode() {
  $('room-note').hidden = true;
  go('StopCode').then(() => go('ShowCode')).catch(tell);
}

// useCode takes the code the other device shows; when that device has
// answered, the State brings the code for both owners to compare.
function useCode() {
  const code = $('room-code').value.trim();
  if (!code || ui.coding) return;
  ui.coding = true;
  $('room-note').className = 'wait';
  $('room-note-text').textContent = 'Looking for the other device…';
  $('room-note').hidden = false;
  render();
  go('UseCode', code).then(() => {
    ui.coding = false;
    $('room-code').value = '';
    $('room-note').hidden = true;
    render();
  }, err => {
    ui.coding = false;
    $('room-note').className = 'wait problem';
    $('room-note-text').textContent = err?.message || String(err);
    render();
  });
}

function answerOffer(accept) {
  if (state?.offer) go('AnswerOffer', state.offer.id, accept, accept && $('in-always').checked).catch(tell);
}

function saveName() {
  const input = $('set-name');
  if (!state || input.value.trim() === state.name) { input.value = state?.name ?? ''; return; }
  go('SetName', input.value).catch(err => { tell(err); input.value = state.name; });
}

$('nav').addEventListener('click', e => {
  const tab = e.target.closest('button')?.dataset.tab;
  if (!tab) return;
  ui.tab = tab;
  if (tab === 'received') go('SeenReceived');
  render();
});
document.addEventListener('click', () => { if (ui.menu) { ui.menu = null; ui.unpairing = false; render(); } });
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (ui.menu) { ui.menu = null; ui.unpairing = false; render(); }
  else if (state?.offer || state?.pairing?.request || state?.code) return;
  else if (ui.label) closeLabel();
  else if (ui.pair) closePair();
});
// A file dropped anywhere but on a device should do nothing, not open in the window.
for (const type of ['dragover', 'drop']) window.addEventListener(type, e => e.preventDefault());

$('refresh').addEventListener('click', () => go('Refresh').catch(tell));
$('pair-open').addEventListener('click', () => openPair('show'));
$('pt-show').addEventListener('click', () => openPair('show'));
$('pt-have').addEventListener('click', () => openPair('have'));
$('pt-code').addEventListener('click', () => openPair('code'));
$('room-show').addEventListener('click', showRoomCode);
$('room-pair').addEventListener('click', useCode);
$('room-code').addEventListener('keydown', e => { if (e.key === 'Enter') useCode(); });
for (const b of document.querySelectorAll('[data-pair-close]')) b.addEventListener('click', closePair);
$('pair-copy').addEventListener('click', () => {
  if (state?.pairing?.link) copy(state.pairing.link).then(() => { $('pair-copy').textContent = 'Copied'; }, tell);
});
$('join').addEventListener('click', join);
$('pair-link').addEventListener('keydown', e => { if (e.key === 'Enter') join(); });
// A failed try's message is about the link that was in the box, so it goes
// when the box changes.
$('pair-link').addEventListener('input', () => { if (!ui.joining) $('join-wait').hidden = true; });
$('label-yes').addEventListener('click', saveLabel);
$('label-no').addEventListener('click', closeLabel);
$('label-name').addEventListener('keydown', e => { if (e.key === 'Enter') saveLabel(); });
$('cf-yes').addEventListener('click', () => go('AnswerPairing', true));
$('cf-no').addEventListener('click', () => go('AnswerPairing', false));
$('code-yes').addEventListener('click', () => go('AnswerCode', true));
$('code-no').addEventListener('click', () => go('AnswerCode', false));
$('in-yes').addEventListener('click', () => answerOffer(true));
$('in-no').addEventListener('click', () => answerOffer(false));
$('open-folder').addEventListener('click', () => go('OpenSaveDir').catch(tell));
$('choose-folder').addEventListener('click', () => go('ChooseSaveDir').catch(tell));
// These three are the web page's only: its bridge answers them, not Go.
$('stop-folder').addEventListener('click', () => go('ForgetSaveDir').catch(tell));
$('folder-allow').addEventListener('click', () => go('AllowSaveDir').catch(tell));
$('folder-later').addEventListener('click', () => go('LaterSaveDir').catch(tell));
$('donate').addEventListener('click', () => go('OpenDonate').catch(tell));
$('set-name').addEventListener('change', saveName);
// The box shows what the system has, so put it back if the change failed.
$('set-autostart').addEventListener('change', e => go('SetAutostart', e.target.checked).catch(err => { e.target.checked = state.autostart; tell(err); }));
$('set-visible').addEventListener('change', e => go('SetVisible', e.target.checked).catch(err => { e.target.checked = state.visible; tell(err); }));
$('set-updates').addEventListener('change', e => go('SetUpdateCheck', e.target.checked).catch(err => { e.target.checked = state.updateCheck; tell(err); }));
$('update-get').addEventListener('click', () => go('GetUpdate').catch(tell));
$('withdrawn-get').addEventListener('click', () => go('GetUpdate').catch(tell));
$('update-hide').addEventListener('click', () => go('HideUpdate').catch(tell));
$('started-quit').addEventListener('click', () => go('Quit').catch(tell));
$('started-hide').addEventListener('click', () => go('HideStarted').catch(tell));
$('blocked-fix').addEventListener('click', () => go('FixFirewall').catch(tell));
$('blocked-hide').addEventListener('click', () => go('HideFirewall').catch(tell));
$('set-name').addEventListener('keydown', e => { if (e.key === 'Enter') e.target.blur(); });

// take draws a State from Go, unless a newer one has already been drawn:
// answers to calls and events can arrive out of order.
function take(s) {
  if (state && s.seq < state.seq) return;
  state = s;
  render();
}

on('state', take);

// Opened from a pairing link: take the link out of the address bar, so it is
// not kept in the history or pasted on by accident, and offer it for pairing.
if (location.hash.startsWith('#pair=')) {
  const link = location.href;
  history.replaceState(null, '', location.pathname);
  openPair('have');
  $('pair-link').value = link;
}
on('say', say);
go('State').then(take, tell);
