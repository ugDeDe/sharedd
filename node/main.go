package main

import (
	"fmt"
	"os"

	"sharedd/node/internal/agent"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" {
			fmt.Println("sharedd-node-agent")
			return
		}
	}
	agent.Run()
}
