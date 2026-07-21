package main

import (
	"context"
	"log"

	"github.com/Yzx7/sacs-chatbots/bootstrap"
)

func main() {
	ctx := context.Background()

	rt, err := bootstrap.Build(ctx)
	if err != nil {
		log.Fatalf("no se pudo arrancar: %v", err)
	}
	defer rt.Close()

	if err := rt.Listen(); err != nil {
		log.Fatalf("servidor detenido: %v", err)
	}
}
