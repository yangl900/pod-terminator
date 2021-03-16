package main

import (
	"log"
	"net/http"
	"os"
)

const statfile = "fail"

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, req *http.Request) {
		if _, err := os.Stat(statfile); err == nil {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		rw.WriteHeader(200)
	})

	mux.HandleFunc("/fail", func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			rw.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if _, err := os.Create(statfile); err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
			rw.Write([]byte(err.Error()))
			return
		}

		rw.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:    ":9527",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServe())
}
