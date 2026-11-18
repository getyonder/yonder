package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"yonder/internal/control"
	"yonder/internal/gui"
	"yonder/internal/nearby"
)

// The interface is plain HTML, CSS and JavaScript, shown in the system's
// web view. Nothing is built or fetched: these files are the app's window.
//
//go:embed frontend
var frontend embed.FS

// The tray icon: the app's icon, and for the macOS menu bar the mark alone,
// which macOS colours itself. Both are drawn from packaging/*.svg.
var (
	//go:embed icons/tray.png
	trayIcon []byte
	//go:embed icons/tray-template.png
	trayTemplate []byte
)

// window is the native window, as the gui package's Shell.
type window struct {
	app   *application.App
	win   *application.WebviewWindow
	notes *notifications.NotificationService
	// svc is set once the Service has started; a second launch can come
	// before that.
	svc atomic.Pointer[gui.Service]
}

func (w *window) Changed(s gui.State) { w.app.Event.Emit("state", s) }
func (w *window) Say(message string)  { w.app.Event.Emit("say", message) }

// Attention makes the window visible without taking the keyboard: a
// question must not land under keys meant for another app.
func (w *window) Attention() {
	w.win.Show()
	w.win.UnMinimise()
}

// open brings the window forward for someone who asked for it.
func (w *window) open() {
	w.win.Show()
	w.win.UnMinimise()
	w.win.Focus()
}

// Notify shows a desktop notification, unless the person is already at the
// window. Clicking it opens the window.
func (w *window) Notify(title, body string) {
	if w.win.IsFocused() {
		return
	}
	go func() {
		// macOS asks the person once whether Yonder may notify them.
		if ok, err := w.notes.RequestNotificationAuthorization(); err != nil || !ok {
			return
		}
		id := strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := w.notes.SendNotification(notifications.NotificationOptions{ID: id, Title: title, Body: body}); err != nil {
			log.Println("showing a notification:", err)
		}
	}()
}

// hiddenFlag starts Yonder in the tray; it is how Yonder is started at login.
const hiddenFlag = "hidden"

func (w *window) Autostart() bool {
	on, _ := w.app.Autostart.IsEnabled()
	return on
}

func (w *window) SetAutostart(on bool) error {
	if !on {
		return w.app.Autostart.Disable()
	}
	return w.app.Autostart.EnableWithOptions(application.AutostartOptions{Arguments: []string{"-" + hiddenFlag}})
}

func (w *window) PickFiles() ([]string, error) {
	return w.app.Dialog.OpenFile().SetTitle("Send files").CanChooseFiles(true).AttachToWindow(w.win).PromptForMultipleSelection()
}

func (w *window) PickFolder(from string) (string, error) {
	return w.app.Dialog.OpenFile().SetTitle("Save received files to").CanChooseFiles(false).CanChooseDirectories(true).CanCreateDirectories(true).SetDirectory(from).AttachToWindow(w.win).PromptForSingleSelection()
}

func (w *window) Open(path string) error { return w.app.Browser.OpenFile(path) }

func (w *window) Browse(url string) error { return w.app.Browser.OpenURL(url) }

// quitWait is how long an ordinary quit gets before the app is stopped
// regardless.
const quitWait = 2 * time.Second

// dropPrefix starts the id of a device's row in the window; the rest is the
// device's ID.
const dropPrefix = "dev-"

// runWindow runs Yonder with its window and tray icon, and returns when it
// is quit. Started hidden, the window waits in the tray.
func runWindow(dir, listen, meeting string, hidden bool) error {
	assets, err := fs.Sub(frontend, "frontend")
	if err != nil {
		return err
	}
	// A running Yonder from before 0.1.30 says nothing when a newer one is
	// started, so this one says it. Started hidden, nobody is there to read
	// it, and the hand-over below does as before.
	if !hidden && olderRuns(dir) {
		return runOlder()
	}
	w := &window{notes: notifications.New()}
	// Two Yonders sharing a config folder would share a tunnel address, so
	// a second launch only brings the first one's window forward.
	instance := sha256.Sum256([]byte(dir))
	w.app = application.New(application.Options{
		Name:        "Yonder",
		Description: "Send files between devices, on the same network or not",
		Assets:      application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Services:    []application.Service{application.NewService(w.notes)},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "app.yonder.c" + hex.EncodeToString(instance[:8]), // D-Bus allows no leading digit
			// The second launch says its version, so that this one can say
			// when it is the older of the two.
			AdditionalData: map[string]string{"version": version},
			OnSecondInstanceLaunch: func(d application.SecondInstanceData) {
				if svc := w.svc.Load(); svc != nil {
					svc.Started(d.AdditionalData["version"])
				}
				w.open()
			},
		},
	})
	// Wails gives every app File, Edit, View, Window and Help menus, with
	// nothing of Yonder's in them. A Mac app has to have a menu bar, and
	// its Edit menu is what makes copy and paste work in a text box, so
	// there it is cut down to what does something; elsewhere there is none.
	if runtime.GOOS == "darwin" {
		menu := w.app.NewMenu()
		menu.AddRole(application.AppMenu)
		menu.AddRole(application.EditMenu)
		menu.AddRole(application.WindowMenu)
		w.app.Menu.Set(menu)
	}
	// The page is laid out for 920 by 620. On Windows the size given is the
	// whole window's, title bar and borders included, which left the page
	// 16 narrower and 38 shorter: "Pair a device" did not fit and scrolled.
	// Its text is also a little taller in Windows's fonts, so with the 38
	// alone it still scrolled by a few pixels; the 32 more are room for that.
	width, height := 920, 620
	if runtime.GOOS == "windows" {
		width, height = width+16, height+38+32
	}
	w.win = w.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Yonder",
		Width:  width,
		Height: height,
		// The window can grow but not shrink below the size it opens at.
		MinWidth:       width,
		MinHeight:      height,
		EnableFileDrop: true,
		Hidden:         hidden,
		URL:            "/",
		Windows:        application.WindowsWindow{DisableMenu: true},
	})

	// Closing the window leaves Yonder receiving, in the tray; Quit there
	// stops it.
	w.win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		w.win.Hide()
		e.Cancel()
	})
	tray := w.app.SystemTray.New()
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(trayTemplate)
	} else {
		tray.SetIcon(trayIcon)
		tray.SetLabel("Yonder") // the tray's name for it; macOS would write it beside the icon
	}
	tray.SetTooltip("Yonder")
	menu := w.app.NewMenu()
	menu.Add("Open Yonder").OnClick(func(*application.Context) { w.open() })
	menu.AddSeparator()
	menu.Add("Quit Yonder").OnClick(func(*application.Context) { w.app.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(w.open)
	// The same for a click on the notification, and on the macOS Dock icon.
	w.notes.OnNotificationResponse(func(notifications.NotificationResult) { w.open() })
	w.app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { w.open() })

	updateURL, withdrawKey := tryout()
	svc, err := gui.Start(gui.Options{Dir: dir, Listen: listen, Meeting: meeting, Kind: nearby.KindApp, Version: version, UpdateURL: updateURL, WithdrawKey: withdrawKey, Quit: w.app.Quit}, w)
	if err != nil {
		return err
	}
	w.svc.Store(svc)
	defer svc.Close()
	// Terminal commands ask this Yonder, and start none of their own.
	if stop, err := control.Serve(control.Path(dir), svc); err != nil {
		log.Println("terminal commands cannot reach this Yonder:", err)
	} else {
		defer stop()
	}
	w.app.RegisterService(application.NewService(svc))

	// Files dropped on a device's row are sent to it.
	w.win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		target := e.Context().DropTargetDetails()
		if target == nil || !strings.HasPrefix(target.ElementID, dropPrefix) {
			return
		}
		if err := svc.Send(strings.TrimPrefix(target.ElementID, dropPrefix), e.Context().DroppedFiles()); err != nil {
			w.Say(err.Error())
		}
	})
	// Wails quits on these signals too, but its quit waits behind an open
	// file chooser for as long as the chooser stays open. Logging out or
	// shutting down must not hang on that, so after quitWait stop anyway.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		time.Sleep(quitWait)
		svc.Close()
		os.Exit(0)
	}()
	return w.app.Run()
}
