// The files Go sees when Yonder runs as a web page. A browser has no file
// system, and Go's WebAssembly build asks for one through a Node-style `fs`
// object, so this is one, kept in memory:
//
//   /yonder/   settings, identity, tunnel keys and paired devices. These are
//              also written to the browser's localStorage, so they are still
//              there on the next visit. Clearing the site's data forgets
//              them, which unpairs this device.
//   /send/     the files picked to send, read from the browser's own File
//              objects piece by piece, never loaded whole.
//   the rest   received files, held in memory until the page is closed, or
//              until the page has written one into a folder (release). Where
//              the page takes a file as it arrives (divert), its pieces go
//              straight there and are never held.
const KEPT = '/yonder/', STORE = 'yonder:';
const O_CREAT = 64, O_EXCL = 128, O_TRUNC = 512;

// path -> { dir } | { chunks, size } | { blob }, each with a time, at. A
// diverted file has a sink for its pieces, and is gone from the start.
const nodes = new Map();
const opened = new Map(); // fd -> { path, node, pos, wrote }
let lastFD = 9, lastBatch = 0;
let taker = null; // see divert

const fail = code => Object.assign(new Error(code), { code });
const clean = path => '/' + path.split('/').filter(Boolean).join('/');
const parent = path => path.slice(0, path.lastIndexOf('/')) || '/';
const find = path => nodes.get(clean(path)) ?? (() => { throw fail('ENOENT'); })();

function mkdirs(path) {
  if (!nodes.has(path)) { if (path !== '/') mkdirs(parent(path)); nodes.set(path, { dir: true, at: Date.now() }); }
}

// bytes is a file's content in one piece.
function bytes(node) {
  if (node.chunks.length !== 1) {
    const all = new Uint8Array(node.size);
    let at = 0;
    for (const c of node.chunks) { all.set(c, at); at += c.length; }
    node.chunks = [all];
  }
  return node.chunks[0];
}

// keep copies a file under /yonder/ to localStorage, or removes it there
// once it is gone. They are all text.
function keep(path) {
  if (!path.startsWith(KEPT)) return;
  try {
    const node = nodes.get(path);
    if (node?.chunks) localStorage.setItem(STORE + path, new TextDecoder().decode(bytes(node)));
    else localStorage.removeItem(STORE + path);
  } catch { /* storage is off or full: this visit still works, the next starts afresh */ }
}

mkdirs('/');
try {
  for (let i = 0; i < localStorage.length; i++) {
    const key = localStorage.key(i);
    if (!key.startsWith(STORE + KEPT)) continue;
    const data = new TextEncoder().encode(localStorage.getItem(key)), path = key.slice(STORE.length);
    mkdirs(parent(path));
    nodes.set(path, { chunks: [data], size: data.length, at: Date.now() });
  }
} catch { /* no storage: start afresh */ }

function stat(node) {
  const size = node.dir ? 0 : node.blob ? node.blob.size : node.size;
  return {
    dev: 0, ino: 0, mode: node.dir ? 0o40755 : 0o100644, nlink: 1, uid: 0, gid: 0, rdev: 0,
    size, blksize: 4096, blocks: Math.ceil(size / 512), atimeMs: node.at, mtimeMs: node.at, ctimeMs: node.at,
    isDirectory: () => !!node.dir,
  };
}

const fd = n => opened.get(n) ?? (() => { throw fail('EBADF'); })();
const nothing = () => {};

// What Go asks of a file system, each returning its answer or throwing an
// error with a code such as ENOENT. Only what Yonder uses is here.
const calls = {
  open(path, flags) {
    path = clean(path);
    let node = nodes.get(path), wrote = false;
    if (node && (flags & O_CREAT) && (flags & O_EXCL)) throw fail('EEXIST');
    if (!node) {
      if (!(flags & O_CREAT) || !nodes.get(parent(path))?.dir) throw fail('ENOENT');
      node = { chunks: [], size: 0, at: Date.now() };
      nodes.set(path, node);
      wrote = true;
    } else if ((flags & O_TRUNC) && node.chunks) {
      node.chunks = []; node.size = 0; wrote = true;
    }
    opened.set(++lastFD, { path, node, pos: 0, wrote });
    return lastFD;
  },
  close(n) {
    const f = fd(n);
    opened.delete(n);
    if (f.wrote && nodes.get(f.path) === f.node) keep(f.path);
  },
  async read(n, buffer, offset, length, position) {
    const f = fd(n), at = position ?? f.pos;
    if (f.node.dir) throw fail('EISDIR');
    if (f.node.gone) throw fail('EIO');
    const part = f.node.blob
      ? new Uint8Array(await f.node.blob.slice(at, at + length).arrayBuffer())
      : bytes(f.node).subarray(at, at + length);
    buffer.set(part, offset);
    if (position == null) f.pos += part.length;
    return part.length;
  },
  // Files are only ever written from start to end, so a write adds a piece.
  async write(n, buffer, offset, length, position) {
    const f = fd(n), node = f.node;
    if (!node.chunks || (position != null && position !== node.size)) throw fail('EINVAL');
    // A received file's first piece: the page is asked, once, to take it.
    if (taker && !node.size && !node.asked && !f.path.startsWith(KEPT)) {
      node.asked = true;
      node.sink = await taker(f.path);
      if (node.sink) node.gone = true;
    }
    if (node.sink) {
      // Go waits for the answer, so the piece needs no copy, and no more
      // arrives than the sink takes. A sink that fails has tidied up.
      try { await node.sink.write(buffer.subarray(offset, offset + length)); } catch { node.sink = null; throw fail('EIO'); }
    } else if (node.gone) throw fail('EIO');
    else node.chunks.push(buffer.slice(offset, offset + length));
    node.size += length;
    f.wrote = true;
    return length;
  },
  // A file is only ever cut shorter, in practice to nothing.
  ftruncate(n, length) {
    const f = fd(n);
    if (!f.node.chunks || f.node.gone || length > f.node.size) throw fail('EINVAL');
    f.node.chunks = length ? [bytes(f.node).slice(0, length)] : [];
    f.node.size = length;
    f.wrote = true;
  },
  stat: path => stat(find(path)),
  lstat: path => stat(find(path)),
  fstat: n => stat(fd(n).node),
  mkdir(path) {
    path = clean(path);
    if (nodes.has(path)) throw fail('EEXIST');
    if (!nodes.get(parent(path))?.dir) throw fail('ENOENT');
    nodes.set(path, { dir: true, at: Date.now() });
  },
  readdir(path) {
    path = clean(path);
    if (!find(path).dir) throw fail('ENOTDIR');
    return [...nodes.keys()].filter(p => p !== '/' && parent(p) === path).map(p => p.slice(p.lastIndexOf('/') + 1));
  },
  async unlink(path) {
    path = clean(path);
    const node = find(path);
    if (node.dir) throw fail('EISDIR');
    nodes.delete(path);
    keep(path);
    // A diverted file goes from where its pieces went too, whole or not.
    await node.sink?.drop();
  },
  rmdir(path) {
    path = clean(path);
    if (!find(path).dir) throw fail('ENOTDIR');
    if (calls.readdir(path).length) throw fail('ENOTEMPTY');
    nodes.delete(path);
  },
  async rename(from, to) {
    const name = to;
    from = clean(from); to = clean(to);
    const node = find(from);
    if (node.dir || nodes.get(to)?.dir) throw fail('EISDIR');
    // Go renames a received file to its own name when it is whole and
    // checked: only then does a diverted one get that name where it went.
    if (node.sink) try { await node.sink.done(name); } catch { node.sink = null; throw fail('EIO'); }
    nodes.delete(from);
    nodes.set(to, node);
    keep(from); keep(to);
  },
  chmod: nothing, fchmod: nothing, chown: nothing, fchown: nothing, lchown: nothing, utimes: nothing, fsync: nothing,
};

// Go's own glue (wasm_exec.js) has put a stand-in at globalThis.fs that only
// knows how to print what Go writes to its output; keep that, add the rest.
const glue = globalThis.fs;
const fs = { ...glue, constants: { O_WRONLY: 1, O_RDWR: 2, O_CREAT, O_EXCL, O_TRUNC, O_APPEND: 1024, O_DIRECTORY: 65536 } };
for (const [name, f] of Object.entries(calls)) {
  fs[name] = (...args) => {
    const done = args.pop();
    if (name === 'write' && args[0] <= 2) return glue.write(...args, done);
    new Promise(resolve => resolve(f(...args))).then(v => done(null, v), e => done(e.code ? e : fail('EIO')));
  };
}
globalThis.fs = fs;

// offer makes the browser's File objects, from a file chooser or a drop,
// into paths that Go can send.
export function offer(files) {
  const batch = ++lastBatch;
  return [...files].map((file, i) => {
    const path = `/send/${batch}/${i}/${file.name}`;
    mkdirs(parent(path));
    nodes.set(path, { blob: file, at: Date.now() });
    return path;
  });
}

// divert gives take(path) each received file that Go starts to write. It
// resolves to a sink for the file's pieces, or to null to leave the file in
// memory. A sink has write(piece), done(path) for when Go gives the whole
// file its name, and drop() for when Go removes it, before or after that;
// one whose write or done fails drops what it has itself.
export function divert(take) { taker = take; }

// received is a file Go has written, as a Blob, or null if there is none.
export function received(path) {
  const node = nodes.get(clean(path));
  return node?.chunks && !node.gone ? new Blob(node.chunks) : null;
}

// release lets go of a received file's content, once the page has saved it
// elsewhere. Its name stays taken, so a later file of that name gets its own.
export function release(path) {
  const node = nodes.get(clean(path));
  if (node?.chunks) { node.chunks = []; node.gone = true; }
}
