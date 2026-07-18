// Command waiveo-feeder is the Wave-1 skeleton for the feeder component: the
// Go process that will source content and hand it to a relay for eventual
// display on a player. For now it only exposes a /healthz probe so the dev
// loop (`make dev`) has something real to build and run in place of the
// Wave-0 stub.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

const addr = "127.0.0.1:7420"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)

	log.Printf("waiveo-feeder listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"component": "waiveo-feeder",
		"status":    "ok",
	})
}
