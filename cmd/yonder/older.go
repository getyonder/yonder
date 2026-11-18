package main

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"net/url"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"yonder/internal/control"
)

// The page of the small window; its styles and fonts are the main window's.
//
//go:embed older
var olderPage embed.FS

// olderRuns is whether a Yonder from before 0.1.30 runs for the folder: one
// that answers terminal commands and does not say its version. Such a copy
// only comes forward when a second one is started, and cannot say that the
// second one is newer (gui.Service.Started), so the second one says it.
func olderRuns(dir string) bool {
	list, err := control.Devices(context.Background(), control.Path(dir))
	return err == nil && list.Running && list.App == ""
}

// runOlder shows a small window that says an older Yonder is running and
// how to quit it, and returns when the window is closed. It starts nothing
// of Yonder: no receiver, no tunnel and no tray icon.
func runOlder() error {
	assets, err := fs.Sub(frontend, "frontend")
	if err != nil {
		return err
	}
	page, err := fs.Sub(olderPage, "older")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/older/", http.StripPrefix("/older/", http.FileServerFS(page)))
	mux.Handle("/", application.BundledAssetFileServer(assets))
	// No SingleInstance: with it this copy would hand over to the older one
	// and end before it could show anything.
	app := application.New(application.Options{
		Name:   "Yonder",
		Assets: application.AssetOptions{Handler: mux},
	})
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:         "Yonder",
		Width:         480,
		Height:        290,
		DisableResize: true,
		URL:           "/older/?v=" + url.QueryEscape(version),
		Windows:       application.WindowsWindow{DisableMenu: true},
	})
	win.RegisterHook(events.Common.WindowClosing, func(*application.WindowEvent) { app.Quit() })
	return app.Run()
}
