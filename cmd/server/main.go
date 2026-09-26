// Command server runs the local execution cost estimation HTTP service.
//
// Endpoints:
//
//	POST /v1/depth/snapshot   install a full depth snapshot
//	POST /v1/depth/updates    apply one incremental update
//	POST /v1/estimates        create a plan from one consistent snapshot
//	GET  /v1/plans/{id}       fetch a plan
//	POST /v1/plans/{id}/settle   settle one slice {"slice_index": n}
//	POST /v1/plans/{id}/cancel   cancel the plan
//
// On startup the service recovers from the last committed state file.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/guoyyit/execution-cost-estimator-go/depth"
	"github.com/guoyyit/execution-cost-estimator-go/model"
	"github.com/guoyyit/execution-cost-estimator-go/service"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	statePath := flag.String("state", "./data/estimator-state.json", "durable state file")
	flag.Parse()

	svc := service.New(service.Config{PersistPath: *statePath})
	if err := svc.Recover(); err != nil {
		log.Fatalf("recovery failed: %v", err)
	}
	defer func() { _ = svc.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/depth/snapshot", handleSnapshot(svc))
	mux.HandleFunc("POST /v1/depth/updates", handleUpdate(svc))
	mux.HandleFunc("POST /v1/estimates", handleEstimate(svc))
	mux.HandleFunc("GET /v1/plans/{id}", handleGetPlan(svc))
	mux.HandleFunc("POST /v1/plans/{id}/settle", handleSettle(svc))
	mux.HandleFunc("POST /v1/plans/{id}/cancel", handleCancel(svc))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("execution-cost-estimator listening on %s (state=%s)", *addr, *statePath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func handleSnapshot(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var v model.DepthView
		if !decode(w, r, &v) {
			return
		}
		if err := svc.HandleSnapshot(model.Snapshot{View: v}); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": v.Seq})
	}
}

func handleUpdate(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var u model.DepthUpdate
		if !decode(w, r, &u) {
			return
		}
		if err := svc.HandleUpdate(u); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": u.Seq})
	}
}

func handleEstimate(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.EstimateRequest
		if !decode(w, r, &req) {
			return
		}
		rep, err := svc.Estimate(req)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusCreated
		if rep.Degraded != nil {
			status = http.StatusOK // created but degraded
		}
		writeJSON(w, status, rep)
	}
}

func handleGetPlan(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := svc.GetPlan(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func handleSettle(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SliceIndex int `json:"slice_index"`
		}
		if !decode(w, r, &body) {
			return
		}
		p, err := svc.Settle(r.PathValue("id"), body.SliceIndex)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func handleCancel(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := svc.Cancel(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, model.ErrEmptyDepth), errors.Is(err, depth.ErrStaleDepth):
		status = http.StatusServiceUnavailable
	case errors.Is(err, model.ErrInsufficientDepth):
		status = http.StatusConflict
	case errors.Is(err, model.ErrPlanNotFound):
		status = http.StatusNotFound
	case errors.Is(err, model.ErrPlanCancelled):
		status = http.StatusConflict
	case errors.Is(err, model.ErrInvalidArgument):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}
