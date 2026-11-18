//go:build js && wasm

// Yonder as a web page: the Service the desktop window drives, compiled to
// WebAssembly and driven by the same interface. The page's bridge.js starts
// this program and passes it the interface's calls. A browser has no file
// system, so the page gives Go one of its own (page/files.js) in which the
// settings, identity and paired devices are kept in the browser's storage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"syscall/js"
	"time"

	"yonder/internal/address"
	"yonder/internal/gui"
	"yonder/internal/nearby"
)

// configDir is the folder, in the page's file system, for this device's
// settings, identity and paired devices.
const configDir = "/yonder"

// page is the web page, as the gui package's Shell. The page itself picks
// the files to send and saves the received ones, because a browser only lets
// a click do those, so most of what a window does is not needed here.
type page struct{ js js.Value }

func (p page) Changed(s gui.State) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	p.js.Call("changed", string(b))
}

func (p page) Say(message string)                { p.js.Call("say", message) }
func (p page) Attention()                        {}
func (p page) Notify(title, body string)         {}
func (p page) Autostart() bool                   { return false }
func (p page) SetAutostart(bool) error           { return errors.New("A web page cannot start by itself.") }
func (p page) PickFiles() ([]string, error)      { return nil, nil }
func (p page) PickFolder(string) (string, error) { return "", nil }
func (p page) Open(string) error                 { return nil }
func (p page) Browse(string) error               { return nil }

// version is the version of Yonder this page was built as, which it tells
// other Yonders; packaging/api.sh sets it from the app's.
var version = "dev"

func main() {
	p := page{js.Global().Get("yonderPage")}
	base := service()
	go opened(base)
	svc, err := gui.Start(gui.Options{Dir: configDir, Meeting: meetingPoint(base), Kind: nearby.KindBrowser, Says: version}, p)
	if err != nil {
		p.js.Call("failed", err.Error())
		return
	}
	// yonderCall(method, args) calls one of the Service's methods. args is
	// a JSON array; the promise it returns resolves to the result as JSON.
	js.Global().Set("yonderCall", js.FuncOf(func(_ js.Value, args []js.Value) any {
		method, in := args[0].String(), args[1].String()
		return promise(func() (string, error) { return call(svc, method, in) })
	}))
	p.js.Call("ready")
	select {}
}

// serviceFile is a file beside the page that can name the service this
// copy of the web app calls, as {"service": "https://name"}. Yonder's own
// web app has one that names none ({}, from packaging/webapp.sh, so that
// the question gets no 404) and calls the service the apps call. A copy
// for a test, or one that somebody hosts with a service of their own, has
// one with a name. It gives whoever serves the page nothing new: they
// choose its code.
const serviceFile = "service.json"

// service is the address of the service this page calls, with no "/" at
// its end: the meeting point is there, as it is for the apps.
func service() string {
	builtIn := "https://" + address.Service
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	at := js.Global().Get("URL").New(serviceFile, js.Global().Get("location").Get("href")).Call("toString").String()
	req, err := http.NewRequestWithContext(ctx, "GET", at, nil)
	if err != nil {
		return builtIn
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return builtIn
	}
	defer resp.Body.Close()
	var named struct {
		Service string `json:"service"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<12)).Decode(&named) != nil {
		return builtIn
	}
	if s := strings.TrimSuffix(named.Service, "/"); strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return s
	}
	return builtIn
}

// meetingPoint is where devices on the same network find each other, and
// where two devices meet by a code: at the service, as for the apps.
func meetingPoint(service string) string {
	return "ws" + strings.TrimPrefix(service, "http") + nearby.Path
}

// opened tells the service, once, that this page was started, which is all
// the service counts of the web app (see cmd/yonder-api, openedPath). No
// answer is needed.
func opened(service string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", service+"/opened", nil)
	if err != nil {
		return
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

var errorType = reflect.TypeFor[error]()

// call runs the Service method with the given name on arguments given as a
// JSON array, and returns what it returns as JSON.
func call(svc *gui.Service, method, in string) (string, error) {
	m := reflect.ValueOf(svc).MethodByName(method)
	if !m.IsValid() {
		return "", fmt.Errorf("Yonder has no %q.", method)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(in), &raw); err != nil || len(raw) != m.Type().NumIn() {
		return "", fmt.Errorf("%s was called with the wrong arguments.", method)
	}
	args := make([]reflect.Value, len(raw))
	for i, r := range raw {
		arg := reflect.New(m.Type().In(i))
		if err := json.Unmarshal(r, arg.Interface()); err != nil {
			return "", fmt.Errorf("%s was called with the wrong arguments.", method)
		}
		args[i] = arg.Elem()
	}
	var result any
	for _, out := range m.Call(args) {
		if out.Type() == errorType {
			if err, _ := out.Interface().(error); err != nil {
				return "", err
			}
			continue
		}
		result = out.Interface()
	}
	b, err := json.Marshal(result)
	return string(b), err
}

// promise runs f without holding up the page, and returns a JavaScript
// Promise of its result.
func promise(f func() (string, error)) js.Value {
	var start js.Func
	start = js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			defer start.Release()
			v, err := f()
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	return js.Global().Get("Promise").New(start)
}
