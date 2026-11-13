package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"

	"marlin-desktop/app"
)

// storePath is the projection file: <config>/marlin-desktop/session.json.
func storePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	d := filepath.Join(dir, "marlin-desktop")
	_ = os.MkdirAll(d, 0o700)
	return filepath.Join(d, "session.json")
}

// configPath is the host config the settings UI edits:
// <config>/marlin-desktop/app-sidecar.toml.
func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "marlin-desktop", "app-sidecar.toml")
}

func main() {
	chat := app.NewChatService(app.LoadStore(storePath()))
	mygo.Bind(chat)
	mygo.Bind(app.NewSettingsService(configPath()))

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
		go app.Connect(chat, win)
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
