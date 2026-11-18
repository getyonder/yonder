// The bridge between the interface and Go. This one is the desktop window's:
// it reaches the Service through Wails. The web version puts its own
// bridge.js in this file's place, with the same exports, so app.js never
// knows which shell it is in.
import { Call, Events, Clipboard } from '/wails/runtime.js';

// web is whether the interface is running as a web page.
export const web = false;
// go calls one of the Service's methods.
export const go = (method, ...args) => Call.ByName('yonder/internal/gui.Service.' + method, ...args);
// on listens for what Go sends by itself: 'state' and 'say'.
export const on = (event, f) => Events.On(event, e => f(e.data));
// copy puts text on the clipboard.
export const copy = text => Clipboard.SetText(text);
