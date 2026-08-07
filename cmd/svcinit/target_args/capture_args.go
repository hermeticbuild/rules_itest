package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	outputFile := os.Getenv("OUTPUT_FILE")
	if outputFile == "" {
		panic("OUTPUT_FILE must be set")
	}

	outputPath := filepath.Join(os.Getenv("TEST_TMPDIR"), outputFile)
	content := fmt.Sprintf("TASK_NAME=%s\nINJECTED_ENV=%s\nARGS=%s\n",
		os.Getenv("TASK_NAME"),
		os.Getenv("INJECTED_ENV"),
		strings.Join(os.Args[1:], "\n"),
	)
	if err := os.WriteFile(outputPath, []byte(content), 0o600); err != nil {
		panic(err)
	}
}
