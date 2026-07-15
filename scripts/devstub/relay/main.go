package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"component":"relay-stub","status":"ok"}`)
	})
	log.Println("relay-stub on :7401")
	log.Fatal(http.ListenAndServe("127.0.0.1:7401", nil))
}
