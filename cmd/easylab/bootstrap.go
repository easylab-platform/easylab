package main

import (
	"log"
	"os"
)

// bootstrapOperator ensures the deployment's operator credential exists in the
// store, so the chart's fixed tokens (EASYVCS_TOKEN / EASYLAB_TOKEN /
// ARTIFACT_TOKEN, all defaulting to the same value) are valid registered
// tokens. The operator is a normal user (user IS the ownership boundary).
//
// Idempotent: the user and the token are created only when absent.
//
// Env: EASYLAB_BOOTSTRAP_TOKEN (the credential value), EASYLAB_BOOTSTRAP_USER
// (default "operator"). An empty token disables bootstrapping.
func (s *server) bootstrapOperator() {
	token := os.Getenv("EASYLAB_BOOTSTRAP_TOKEN")
	if token == "" {
		return
	}
	username := envOrStr("EASYLAB_BOOTSTRAP_USER", "operator")

	u, err := s.cs.GetUserByUsername(username)
	if err != nil {
		u, err = s.cs.CreateUser(username, "EasyLab Operator")
		if err != nil {
			if u2, gerr := s.cs.GetUserByUsername(username); gerr == nil {
				u = u2
			} else {
				log.Printf("bootstrap operator %q: %v", username, err)
				return
			}
		}
	}
	// Register the credential if it is not already known.
	if _, err := s.cs.LookupToken(token); err == nil {
		return
	}
	if _, err := s.cs.CreateToken(token, u.ID, "write"); err != nil {
		if _, lerr := s.cs.LookupToken(token); lerr == nil {
			return
		}
		log.Printf("bootstrap operator token: %v", err)
	}
}
