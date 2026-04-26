package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Need to supply a config")
		os.Exit(1)
	}

	fmt.Printf("Config file is: %s", os.Args[1])
}

func getGreeting() string {
	return "Hello, World!"
}
