package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	env := os.Environ()
	sort.Strings(env)

	for _, entry := range env {
		if !strings.HasPrefix(entry, "TARGET_ARGS_") {
			continue
		}
		fmt.Println(entry)
	}
	fmt.Printf("ARGS=%q\n", os.Args[1:])
}
