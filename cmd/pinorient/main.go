package main

import (
	"log"

	"github.com/pinorient/pinorient"
)

func main() {
	app := pinorient.New()

	if err := pinorient.Setup(app); err != nil {
		log.Fatal(err)
	}

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
