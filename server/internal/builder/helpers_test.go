// SPDX-License-Identifier: AGPL-3.0-only

package builder_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/agentupdate"
)

func mount(h *agentupdate.Handler) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/agent/v1", h.Mount)
	return r
}

func decodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatal(err)
	}
}
