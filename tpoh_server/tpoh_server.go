package main

import (
    "fmt"
    . "github.com/drewlesueur/cc"
    "github.com/drewlesueur/stucco"
)

// Client certs
// version 1 relays
// version 2 relays...

var E = stucco.E
var GlobalState = stucco.GlobalState

func main() {
    _ = Spawn
    E(".yoWorld say")
    fmt.Println("hello world...")
    
}


func ReadFile() {
    
}