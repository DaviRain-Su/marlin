package main

import (
	"log"

	"github.com/egoist/mygo"

	"marlin-desktop/app"
)

func main() {
	chat := app.NewChatService()
	mygo.Bind(chat)

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
