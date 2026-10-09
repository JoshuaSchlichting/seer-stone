package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend
var assets embed.FS

func main() {
	service, err := NewDatabaseService()
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()

	app := application.New(application.Options{
		Name:        "Seer Stone",
		Description: "A desktop SQL client for CockroachDB",
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
		Services: []application.Service{
			application.NewService(service),
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "main",
		Title:  "Seer Stone — SQL Client",
		Width:  1360,
		Height: 880,
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
