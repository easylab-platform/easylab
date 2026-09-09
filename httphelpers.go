package opsext

import (
	"encoding/json"
	"net/http"
)

// writeJSON / writeErr are retained for the (now-dead) HTTP handler surface
// which stays compiled behind the package; embedded mode never serves them.
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{"ok": false, "error": msg})
}
