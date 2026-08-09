package main

import (
	"encoding/json"
	"os"

	"mcp-local-hub/internal/gui"
)

func main() {
	if err := json.NewEncoder(os.Stdout).Encode(gui.RecoveryWireContract()); err != nil {
		panic(err)
	}
}
