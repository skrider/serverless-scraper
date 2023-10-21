package main

import (
	"log"
	"net/http"

	"github.com/passage-inc/chatassist/packages/vercel/api/continue_convo_go"
	"github.com/passage-inc/chatassist/packages/vercel/api/initialize_convo_go"
	"github.com/passage-inc/chatassist/packages/vercel/api/scrape"
)

func main() {
	http.HandleFunc("/scrape", scrape.Handler)
	http.HandleFunc("/continue_convo", continue_convo_go.Handler)
	http.HandleFunc("/initialize_convo", initialize_convo_go.Handler)
	log.Output(1, "up")
	http.ListenAndServe(":3001", nil)

}
