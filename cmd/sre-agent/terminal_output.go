package main

import (
	"io"
	"os"
)

func isTerminalOutput(output io.Writer) bool {
	file, ok := output.(*os.File)
	return ok && isTerminal(file)
}
