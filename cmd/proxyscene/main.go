package main

import (
	"fmt"
	"os"

	"proxyscene/internal/manager"
)

func main() {
	app := manager.NewApp(manager.DefaultConfig())
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}
}
