// Command waiveo-relay is the Wave-1 skeleton for the relay component: a Go
// process that will speak the relay/1 protocol (contracts/relay-1.md) to an
// app peer and the player over its own future channels. For now it only
// exposes a /healthz probe so the dev loop (`make dev`) has something real
// to build and run in place of the Wave-0 stub.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

const addr = "127.0.0.1:7421"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)

	log.Printf("waiveo-relay listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"component": "waiveo-relay",
		"status":    "ok",
	})
}
