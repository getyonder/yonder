// The bridge between the interface and Go, for Yonder as a web page. It
// takes the place of the desktop window's bridge.js and has the same
// exports. Go runs in the page as WebAssembly (yonder.wasm); what a browser
// only lets a click do, choosing files and saving them, the page does itself.
import './wasm_exec.js';
import { divert, offer, received, release } from './files.js';

// web is whether the interface is running as a web page.
export const web = true;

// website is Yonder's website, which the web app only links to (the same
// address as address.Website in the Go code).
const website = 'https://getyonder.org/';

const listeners = { state: [], say: [] };
// on listens for what Go sends by itself: 'state' and 'say'.
export const on = (event, f) => listeners[event].push(f);
const emit = (event, data) => { for (const f of listeners[event]) f(data); };

let state = null; // the newest State, for the received files' paths
let fromGo = [];  // its transfers as Go sent them: show adds the held ones
// moving is whether a transfer is under way.
const moving = () => !!state?.transfers.some(t => t.phase === 'waiting' || t.phase === 'moving');
let ready, failed;
const started = new Promise((resolve, reject) => { ready = resolve; failed = reject; });
started.catch(() => {}); // told to the person by notice, below

// What Go calls in the page.
globalThis.yonderPage = {
  ready,
  failed: message => failed(new Error(message)),
  changed(json) { state = JSON.parse(json); fromGo = state.transfers; track(); show(); awake(); keepAll(); },
  say: message => emit('say', message),
};

const call = async (method, ...args) => {
  await started;
  try {
    return JSON.parse(await globalThis.yonderCall(method, JSON.stringify(args)));
  } finally {
    // The first answer is what the page draws itself from: see shell.css.
    document.documentElement.classList.add('ready');
  }
};

// go calls one of the Service's methods.
export const go = (method, ...args) => {
  if (method === 'PickAndSend') return pickFiles().then(paths => call('Send', args[0], paths));
  if (method === 'OpenReceived') return Promise.resolve(save(state?.received[args[0]]?.path));
  if (method === 'SaveTransfer') return Promise.resolve((got.get(args[0]) ?? []).forEach(save)).then(show);
  if (method === 'DismissTransfer') { held.delete(args[0]); show(); }
  if (method === 'ChooseSaveDir') return chooseFolder();
  if (method === 'ForgetSaveDir') return forgetFolder();
  if (method === 'AllowSaveDir') return allowFolder();
  if (method === 'LaterSaveDir') return Promise.resolve(later = true).then(show);
  // The website's donate page, in a new tab.
  if (method === 'OpenDonate') return Promise.resolve(window.open(website + 'donate/', '_blank', 'noopener'));
  return call(method, ...args);
};

// copy puts text on the clipboard.
export const copy = async text => {
  if (navigator.clipboard) return navigator.clipboard.writeText(text);
  // A page not served over HTTPS has no clipboard; this older way works there.
  const box = Object.assign(document.createElement('textarea'), { value: text });
  document.body.append(box);
  box.select();
  const copied = document.execCommand('copy');
  box.remove();
  if (!copied) throw new Error('Could not copy the link. Select it and copy it by hand.');
};

// pickFiles opens the browser's file chooser and resolves to the chosen
// files as paths Go can read; none if the person backed out.
function pickFiles() {
  return new Promise(resolve => {
    const input = Object.assign(document.createElement('input'), { type: 'file', multiple: true, hidden: true });
    const done = () => { input.remove(); resolve(offer(input.files)); };
    input.addEventListener('change', done);
    input.addEventListener('cancel', done);
    document.body.append(input);
    input.click();
  });
}

// save hands a received file to the browser as a download.
const saved = new Set(); // the paths of the received files handed over so far
function save(path) {
  const blob = path && received(path);
  if (!blob) return;
  saved.add(path);
  const a = Object.assign(document.createElement('a'), { href: URL.createObjectURL(blob), download: base(path) });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 60000);
}

// Where the browser can give a page a folder (Chrome, Edge and the like on
// a computer), the person may choose one in Settings, and each received
// file is then written into it piece by piece as it arrives (take), or,
// if it arrived in memory, when it is there whole (keepOne). The folder is
// remembered in the browser's IndexedDB, the only store that takes one. A
// browser may want a new yes for it on a later visit; until it has one,
// files wait for the Save button as they do without a folder.
const canPick = !!globalThis.showDirectoryPicker && !!globalThis.indexedDB;
let folder = null, allowed = false, later = false; // later: "Not now" was pressed
const kept = new Set();    // the paths of the received files written to the folder
const refused = new Set(); // those the folder would not take, left to the Save button
const queued = new Set();  // those waiting to be written
let keeping = Promise.resolve(); // one file at a time, so that two never take one name

// store runs one request on the page's IndexedDB store.
const store = (mode, f) => new Promise((resolve, reject) => {
  const open = indexedDB.open('yonder', 1);
  open.onupgradeneeded = () => open.result.createObjectStore('kept');
  open.onerror = () => reject(open.error);
  open.onsuccess = () => {
    const db = open.result, request = f(db.transaction('kept', mode).objectStore('kept'));
    request.onsuccess = () => { db.close(); resolve(request.result); };
    request.onerror = () => { db.close(); reject(request.error); };
  };
});

// show gives the interface the newest State, with what only the page knows
// added: the folder, and which received files are in it.
function show() {
  if (!state) return;
  if (canPick) state.folder = { name: folder?.name ?? '', saving: !!folder && allowed, ask: !!folder && !allowed && !later };
  for (const f of state.received) f.kept = kept.has(f.path);
  if (folder && allowed) held.clear();
  const live = new Set(fromGo.map(t => t.id));
  state.transfers = [...fromGo, ...[...held.values()].filter(t => !live.has(t.id))];
  // unsaved is how many of a finished receive's files still wait for Save.
  for (const t of state.transfers) {
    if (held.has(t.id)) t.unsaved = got.get(t.id).filter(path => !saved.has(path)).length;
  }
  emit('state', state);
}

async function chooseFolder() {
  let dir;
  try {
    dir = await showDirectoryPicker({ id: 'yonder', mode: 'readwrite' });
  } catch (err) {
    if (err.name === 'AbortError') return; // the person backed out
    throw err;
  }
  folder = dir; allowed = true; later = false;
  refused.clear();
  show();
  keepAll();
  await store('readwrite', s => s.put(dir, 'folder')).catch(() => emit('say', 'This browser could not remember the folder. It is used until this page is closed.'));
}

async function forgetFolder() {
  folder = null; allowed = false;
  show();
  await store('readwrite', s => s.delete('folder')).catch(() => {});
}

// allowFolder asks the browser for its yes again; it must come from a click.
async function allowFolder() {
  if (!folder) return;
  allowed = await folder.requestPermission({ mode: 'readwrite' }) === 'granted';
  refused.clear();
  show();
  keepAll();
}

// free is name, or "name (1).ext" and so on as the desktop app has it,
// whichever the folder does not hold yet.
async function free(dir, name) {
  const dot = name.lastIndexOf('.'), stem = dot > 0 ? name.slice(0, dot) : name, ext = dot > 0 ? name.slice(dot) : '';
  for (let i = 0; ; i++) {
    const candidate = i ? `${stem} (${i})${ext}` : name;
    try {
      await dir.getFileHandle(candidate);
    } catch (err) {
      if (err.name === 'NotFoundError') return candidate;
      if (err.name !== 'TypeMismatchError') throw err; // not: a folder has the name
    }
  }
}

// put writes a blob into the folder under a free name, which it returns.
async function put(dir, name, blob) {
  name = await free(dir, name);
  const file = await dir.getFileHandle(name, { create: true });
  try {
    // The browser writes beside the file and puts the content in its place
    // at close, so a write that fails leaves no half of a file.
    const writer = await file.createWritable();
    await writer.write(blob);
    await writer.close();
  } catch (err) {
    await dir.removeEntry(name).catch(() => {});
    throw err;
  }
  return name;
}

const base = path => path.slice(path.lastIndexOf('/') + 1);

// take opens a file in the folder for a received file that Go starts to
// write, so that its pieces never fill the page's memory (see divert in
// files.js). The file has Go's own name for a part until Go names it. With
// no folder, or one that will not open a file, it resolves to null and the
// file arrives in memory as before.
async function take(path) {
  const dir = folder;
  if (!dir || !allowed) return null;
  let name = base(path), made = false, file, writer, whole = null;
  try {
    name = await free(dir, name);
    file = await dir.getFileHandle(name, { create: true });
    made = true;
    writer = await file.createWritable();
  } catch (err) {
    if (made) await dir.removeEntry(name).catch(() => {});
    if (err.name === 'NotAllowedError') { allowed = false; later = false; show(); }
    return null;
  }
  const drop = async () => {
    await writer.abort().catch(() => {}); // closed already, if it was whole
    await dir.removeEntry(name).catch(() => {});
    if (whole) { kept.delete(whole); saved.delete(whole); show(); }
  };
  // Go gets the error and ends the receive; nothing of the file stays.
  const stop = async err => {
    await drop();
    if (err.name === 'NotAllowedError') { allowed = false; later = false; show(); }
    emit('say', `Could not write a file into the folder "${dir.name}": ${err.message} It was not received.`);
    throw err;
  };
  const done = async to => {
    await writer.close();
    try {
      const final = await free(dir, base(to));
      await file.move(final);
      name = final;
    } catch {
      // A browser that will not rename a file there: a copy within the
      // folder, from its disk and not through the page's memory.
      const part = name;
      name = await put(dir, base(to), await file.getFile());
      await dir.removeEntry(part).catch(() => {});
    }
    whole = to;
    kept.add(to);
    saved.add(to);
  };
  return {
    write: piece => writer.write(piece).catch(stop),
    // In the queue of keepOne, so that two files never take one name.
    done(to) {
      const named = keeping.then(() => done(to));
      keeping = named.catch(() => {});
      return named.catch(stop);
    },
    drop,
  };
}
if (canPick) divert(take);

// keepAll writes the received files not yet saved into the folder.
function keepAll() {
  if (!folder || !allowed) return;
  for (const { path } of state?.received ?? []) {
    if (saved.has(path) || refused.has(path) || queued.has(path)) continue;
    queued.add(path);
    keeping = keeping.then(() => keepOne(path));
  }
}

async function keepOne(path) {
  const dir = folder, blob = received(path);
  try {
    if (!dir || !allowed || !blob || saved.has(path)) return;
    await put(dir, base(path), blob);
    kept.add(path);
    saved.add(path);
    release(path);
  } catch (err) {
    // The browser took its yes back: ask again. Anything else is this file's.
    if (err.name === 'NotAllowedError') { allowed = false; later = false; } else refused.add(path);
    emit('say', `Could not save ${base(path)} in the folder "${dir.name}": ${err.message} Press Save to keep it.`);
  } finally {
    queued.delete(path);
    show();
  }
}

if (canPick) store('readonly', s => s.get('folder')).then(async dir => {
  if (!dir) return;
  folder = dir;
  allowed = await dir.queryPermission({ mode: 'readwrite' }) === 'granted';
  show();
  keepAll();
}).catch(() => { /* no store: no folder is remembered */ });

// Go takes a finished transfer off the Devices page after a few seconds.
// Here a finished receive is the person's cue to save what came, so the
// page holds it there, with a Save button for its files, until it is
// dismissed or the person goes to another tab. Not when a folder takes the
// files: then nothing is left to do.
const seen = new Set(); // the received files' paths already put to a transfer
const got = new Map();  // a receive's id -> the paths of the files it brought
const held = new Map(); // a finished receive's id -> the transfer, as Go last sent it
function track() {
  const fresh = state.received.map(f => f.path).filter(path => !seen.has(path));
  for (const path of fresh) seen.add(path);
  // One receive runs at a time, so a new file is the newest receive's.
  const receives = fromGo.filter(t => !t.sending);
  const newest = receives[receives.length - 1];
  if (fresh.length && newest) got.set(newest.id, [...got.get(newest.id) ?? [], ...fresh]);
  for (const t of receives) if (t.phase === 'done' && got.has(t.id)) held.set(t.id, t);
}
document.getElementById('nav').addEventListener('click', e => {
  const tab = e.target.closest('[data-tab]')?.dataset.tab;
  if (!tab || tab === 'devices' || !held.size) return;
  held.clear();
  show();
});

// Leaving the page ends Yonder in it: a transfer under way stops and
// received files not yet saved are gone. So the browser is asked to check
// with the person first when there is either. The words it shows are its
// own, and a phone's browser does not always ask.
window.addEventListener('beforeunload', e => {
  const unsaved = state?.received.some(f => !saved.has(f.path));
  if (!moving() && !unsaved) return;
  e.preventDefault();
  e.returnValue = ''; // what older browsers go by
});

// A phone that locks its screen stops the page, and a transfer with it. So
// the screen is asked to stay on while one is under way and the page is
// shown; a browser takes the lock back when the page is hidden. Not every
// browser has this, and one that has it may refuse (a low battery): the
// transfer's own line says to keep the screen on.
let lock = null, asking = false;
async function awake() {
  if (asking || !navigator.wakeLock) return;
  asking = true;
  try {
    for (;;) {
      const want = moving() && document.visibilityState === 'visible';
      if (want === !!lock) break;
      if (want) {
        const got = await navigator.wakeLock.request('screen');
        got.addEventListener('release', () => { if (lock === got) lock = null; });
        lock = got;
      } else {
        const held = lock;
        lock = null;
        await held.release();
      }
    }
  } catch { /* refused: the transfer goes on, with the screen left to the person */ } finally {
    asking = false;
  }
}
document.addEventListener('visibilitychange', awake);

// Files dropped on a device's row are sent to it, as in the desktop window.
const dropRow = e => e.target.closest?.('[data-file-drop-target]');
let over = null;
function mark(row) {
  if (row === over) return;
  over?.classList.remove('file-drop-target-active');
  row?.classList.add('file-drop-target-active');
  over = row;
}
document.addEventListener('dragover', e => mark(dropRow(e)));
document.addEventListener('dragleave', e => { if (!e.relatedTarget) mark(null); });
document.addEventListener('drop', e => {
  const row = dropRow(e);
  mark(null);
  if (!row || !e.dataTransfer?.files.length) return;
  e.preventDefault();
  call('Send', row.id.slice('dev-'.length), offer(e.dataTransfer.files)).catch(err => emit('say', err.message));
});

// The mark in the header leads to the homepage, as the homepage's own does.
// In the desktop window it is not a link, so it is made one here.
const logo = document.querySelector('.mark');
const home = Object.assign(document.createElement('a'), { className: 'mark', href: website });
home.append(...logo.childNodes);
logo.replaceWith(home);

// shell.css covers the page with "Starting Yonder…" from the first moment
// it is shown until call uncovers it. If Yonder cannot run, notice covers
// it for good with the reason.
started.catch(err => {
  const notice = Object.assign(document.createElement('div'), { className: 'notice', textContent: `Yonder could not start: ${err.message}` });
  notice.setAttribute('role', 'alert');
  document.body.append(notice);
});

// alone resolves to whether this is the only tab with Yonder open. Two would
// share one identity and one tunnel address, and take each other's files.
function alone() {
  if (!globalThis.BroadcastChannel) return Promise.resolve(true);
  const tabs = new BroadcastChannel('yonder');
  return new Promise(resolve => {
    tabs.onmessage = e => { if (e.data === 'here') resolve(false); };
    tabs.postMessage('anyone?');
    setTimeout(() => {
      tabs.onmessage = e => { if (e.data === 'anyone?') tabs.postMessage('here'); };
      resolve(true);
    }, 300);
  });
}

async function start() {
  if (!await alone()) throw new Error('it is already open in another tab. Use that one, or close it and reload this page.');
  if (!globalThis.WebAssembly) throw new Error('this browser is too old for it.');
  const program = new Go();
  program.env = { HOME: '/home' };
  const { instance } = await WebAssembly.instantiateStreaming(fetch('yonder.wasm'), program.importObject);
  // run returns only if Go stops, which it should not.
  program.run(instance).then(() => failed(new Error('it stopped unexpectedly. Reload the page.')));
}
start().catch(failed);
