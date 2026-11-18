// The small window of a newer Yonder that found an older one running. The
// version is this copy's own, given in the address; the older copy's is not
// known, because it does not say it.
import { Application } from '/wails/runtime.js';

const version = new URLSearchParams(location.search).get('v') || '';
document.getElementById('version').textContent = version;
document.getElementById('text').textContent =
  `This is Yonder ${version}. Only one Yonder can run at a time, so this one has not started. ` +
  'Quit the older one from its icon in the tray or menu bar (Quit Yonder), then start Yonder again.';
document.getElementById('close').addEventListener('click', () => Application.Quit());
