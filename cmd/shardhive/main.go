package main

import "fmt"

// version is injected at build time via ldflags, e.g.
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.0.1-dev"

func main() {
	fmt.Println("shardhive", version)
}
