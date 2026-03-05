package main

import (
	"encoding/json"
	"net/http"
)

func main() {
	h := http.NewServeMux()
	h.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	h.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"content": `{"job_category":"Programmazione","role":"Software Engineer","company":"Unknown","seniority":"mid","location":"remote","remote_type":"remote","tech_stack":["go"],"contract_type":"","salary":"","language":"it","summary_it":"Annuncio processato dal mock LLM.","confidence":0.5}`,
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	_ = http.ListenAndServe(":8080", h)
}
