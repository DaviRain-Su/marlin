package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/egoist/mygo"

	"marlin-desktop/app"
)

// storeDir is the directory holding the session registry and the
// per-session projection files: <config>/marlin-desktop.
func storeDir() string {
	if dir := os.Getenv("MARLIN_DATA_DIR"); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
		return dir
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	d := filepath.Join(dir, "marlin-desktop")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// configPath is the host config the settings UI edits:
// <config>/marlin-desktop/app-sidecar.toml.
func configPath() string {
	if dir := os.Getenv("MARLIN_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "app-sidecar.toml")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "marlin-desktop", "app-sidecar.toml")
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var host sync.WaitGroup
	// OnQuit runs after the UI event loop stops and must finish cleanup
	// synchronously. Cancellation stops the supervisor before its host.
	mygo.App.OnQuit(func() {
		cancel()
		host.Wait()
	})
	registry := app.LoadSessionRegistry(filepath.Join(storeDir(), "registry.json"))
	chat := app.NewChatService(app.LoadStore(storeDir()), registry)
	mygo.Bind(chat)
	mygo.Bind(app.NewSettingsService(configPath(), chat))
	mygo.Bind(app.NewProjectService(configPath(), chat))

	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:           "Marlin",
			Width:           960,
			Height:          640,
			MinWidth:        480,
			MinHeight:       360,
			BackgroundColor: "light-dark(#f6f7f9, #0f1115)", // the page's --bg in src/style.css
			// Opens where the user left it last time.
			StateKey: "main",
			// The frontend: devUrl during `mygo dev`, frontendDist
			// embedded by `mygo build` otherwise (see mygo.config.ts).
			URL: "/",
		})
		// The sidecar host lives for the app's lifetime; its journal keeps
		// turns alive across its own restarts.
		host.Add(1)
		go func() {
			defer host.Done()
			app.Connect(ctx, chat, win)
		}()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
