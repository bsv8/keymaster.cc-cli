// Package admin download management: per-seed execution list, inspection,
// redownload registration and current-execution switching.
//
// A pure buyer (km <seed-hash>) has no seller catalog, so the Web Admin must
// also serve the client data directory (~/.km/client). Each seed owns:
//
//	scheduler/tasks/<seed>.<exec>.json   per-execution snapshot + outbox
//	scheduler/tasks/<seed>.current        current execution pointer (plain text)
//
// Host pool keys are always exec-prefixed ("e1/<seed>/pool/1"); there is no
// bare-ID branch. Old executions are never renamed or moved; a new execution
// gets a fresh snapshot file and the pointer is switched atomically. Any
// execution can be made current again, so an old execution's delivery, close
// and refund work keeps a real handling entry instead of a read-only audit.
package admin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	masterseed "github.com/bsv8/MasterSeed"

	"github.com/bsv8/keymaster.cc-cli/internal/host"
	"github.com/bsv8/keymaster.cc-cli/internal/lock"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
)

func validSeedHash(seed string) bool {
	if len(seed) != 64 {
		return false
	}
	raw, err := hex.DecodeString(seed)
	return err == nil && len(raw) == 32
}

func clientSeedDir(clientDir, seed string) string {
	return filepath.Join(clientDir, seed)
}

func openSeedStore(clientDir, seed string) (*store.Store, error) {
	return store.Open(clientSeedDir(clientDir, seed))
}

// listClientSeeds scans the client directory for seed subdirectories that own
// at least one execution snapshot or a current pointer.
func listClientSeeds(clientDir string) ([]string, error) {
	entries, err := os.ReadDir(clientDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() || !validSeedHash(entry.Name()) {
			continue
		}
		s, err := store.Open(filepath.Join(clientDir, entry.Name()))
		if err != nil {
			continue
		}
		execs, _ := host.ListExecs(s, entry.Name())
		_, hasCurrent, _ := host.CurrentExecID(s, entry.Name())
		if len(execs) > 0 || hasCurrent {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

type execSummary struct {
	ExecID    string  `json:"execId"`
	Current   bool    `json:"current"`
	Revision  int64   `json:"revision"`
	Execution string  `json:"execution"`
	Funds     string  `json:"funds"`
	Completed int     `json:"completed"`
	Total     int     `json:"total"`
	Failure   *string `json:"failure,omitempty"`
	Pools     int     `json:"pools"`
	Outbox    int     `json:"outbox"`
}

type downloadView struct {
	SeedHash string        `json:"seedHash"`
	Current  string        `json:"current,omitempty"`
	Execs    []execSummary `json:"execs"`
}

func summarizeExec(state scheduler.State, outbox []host.OutboxEntry, current bool, execID string) execSummary {
	view := scheduler.ProjectView(state)
	summary := execSummary{
		ExecID:    execID,
		Current:   current,
		Revision:  state.Revision,
		Execution: string(view.Execution),
		Funds:     string(view.Funds),
		Completed: view.Completed,
		Total:     view.Total,
		Pools:     len(state.Pools),
		Outbox:    len(outbox),
	}
	if view.Failure != nil {
		code := string(view.Failure.Code)
		if view.Failure.Required != "" {
			code += " required=" + view.Failure.Required
		}
		summary.Failure = &code
	}
	return summary
}

// downloadJob tracks one admin-started background download. It is the
// in-process half of single-flight: the file guard covers other processes
// (including an interactive `km <seed>` run), the jobs map covers background
// jobs in this process.
type downloadJob struct {
	SeedHash  string    `json:"seedHash"`
	ExecID    string    `json:"execId"`
	OutDir    string    `json:"outDir"`
	StartedAt time.Time `json:"startedAt"`
	Done      bool      `json:"done"`
	ExitCode  int       `json:"exitCode"`
	// cancel stops the background run; unexported, never serialized.
	cancel context.CancelFunc
}

// jobStatus reports the current job for a seed, if any.
func (s *Server) jobStatus(seed string) *downloadJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	job, ok := s.jobs[seed]
	if !ok {
		return nil
	}
	snapshot := *job
	return &snapshot
}

// startJob registers a background download and drives it to completion. The
// caller must hold downloadsMu and must have switched the pointer already;
// registration happens before the goroutine starts so concurrent requests
// observe the active job. It returns a snapshot copy: the background goroutine
// keeps mutating the shared entry, so the shared pointer must never be handed
// to the JSON encoder. ok is false when the server is shutting down; the
// caller must then refuse the start instead of launching an orphan job.
func (s *Server) startJob(seed, execID, outDir string) (snapshot downloadJob, ok bool) {
	if s.closing {
		return downloadJob{}, false
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	job := &downloadJob{SeedHash: seed, ExecID: execID, OutDir: outDir, StartedAt: time.Now(), cancel: cancel}
	s.jobsMu.Lock()
	s.jobs[seed] = job
	snapshot = *job
	s.jobsMu.Unlock()
	s.jobsWg.Add(1)
	go func() {
		defer s.jobsWg.Done()
		defer cancel()
		code := s.options.StartDownload(jobCtx, seed, execID, outDir)
		s.jobsMu.Lock()
		job.Done = true
		job.ExitCode = code
		s.jobsMu.Unlock()
	}()
	return snapshot, true
}

func (s *Server) seedStore(seed string) (*store.Store, error) {
	if s.options.ClientDir == "" {
		return nil, errors.New("client directory is not configured")
	}
	if !validSeedHash(seed) {
		return nil, fmt.Errorf("invalid seed hash %q", seed)
	}
	return openSeedStore(s.options.ClientDir, seed)
}

func (s *Server) handleDownloads(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}
	if s.options.ClientDir == "" {
		http.Error(writer, "client directory is not configured", http.StatusNotFound)
		return
	}
	seeds, err := listClientSeeds(s.options.ClientDir)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]downloadView, 0, len(seeds))
	for _, seed := range seeds {
		st, err := openSeedStore(s.options.ClientDir, seed)
		if err != nil {
			continue
		}
		current, _, _ := host.CurrentExecID(st, seed)
		execs, _ := host.ListExecs(st, seed)
		summaries := make([]execSummary, 0, len(execs))
		for _, id := range execs {
			state, outbox, err := host.LoadExecSnapshot(st, seed, id)
			if err != nil {
				continue
			}
			summaries = append(summaries, summarizeExec(state, outbox, id == current, id))
		}
		views = append(views, downloadView{SeedHash: seed, Current: current, Execs: summaries})
	}
	writeJSON(writer, map[string]any{"downloads": views})
}

// handleDownloadRoute dispatches /api/downloads/{seed}, .../redownload and
// .../switch under one prefix registration.
func (s *Server) handleDownloadRoute(writer http.ResponseWriter, request *http.Request) {
	rest := strings.TrimPrefix(request.URL.Path, "/api/downloads/")
	if strings.HasSuffix(rest, "/redownload") {
		s.handleRedownload(writer, request)
		return
	}
	if strings.HasSuffix(rest, "/switch") {
		s.handleSwitch(writer, request)
		return
	}
	if strings.HasSuffix(rest, "/job") {
		s.handleDownloadJob(writer, request)
		return
	}
	s.handleDownload(writer, request)
}

func (s *Server) handleDownload(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}
	seed := strings.TrimPrefix(request.URL.Path, "/api/downloads/")
	if seed == "" || strings.Contains(seed, "/") || !validSeedHash(seed) {
		http.Error(writer, "invalid seed hash", http.StatusBadRequest)
		return
	}
	st, err := s.seedStore(seed)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	current, _, err := host.CurrentExecID(st, seed)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	execs, err := host.ListExecs(st, seed)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	summaries := make([]execSummary, 0, len(execs))
	eligible := ""
	for _, id := range execs {
		state, outbox, err := host.LoadExecSnapshot(st, seed, id)
		if err != nil {
			continue
		}
		summaries = append(summaries, summarizeExec(state, outbox, id == current, id))
		if id == current {
			view := scheduler.ProjectView(state)
			if ok, reason := host.RedownloadEligibility(state, view); ok {
				eligible = "eligible"
			} else {
				eligible = reason
			}
		}
	}
	writeJSON(writer, map[string]any{
		"seedHash": seed, "current": current, "execs": summaries,
		"redownload": eligible,
		"job":        s.jobStatus(seed),
		"hint":       "redownload creates a new execution and starts it in the background",
	})
}

// reuseFromStaging rebuilds the kernel resume lists from the verified staging
// area: the ordered block hashes come from the staged seed bytes, and a block
// counts as reused only when its staged payload hashes to its seed entry.
// Staging is shared across executions and its payloads are hash-verified, so
// sharing them never mixes executions: journals and ledger pools stay
// exec-prefixed while content bytes stay content-addressed.
func reuseFromStaging(ctx context.Context, st *store.Store, seedHash string) (ordered, reused []string, ok bool) {
	seed, err := st.Load("staging/seed")
	if err != nil || len(seed) == 0 {
		return nil, nil, false
	}
	expected, err := masterseed.ParseDigestHex(seedHash)
	if err != nil {
		return nil, nil, false
	}
	if _, err := masterseed.VerifySeed(ctx, bytes.NewReader(seed), expected); err != nil {
		return nil, nil, false
	}
	if uint64(len(seed))%uint64(masterseed.DigestSize) != 0 {
		return nil, nil, false
	}
	blockCount := uint64(len(seed)) / uint64(masterseed.DigestSize)
	ordered = make([]string, 0, blockCount)
	seen := map[string]bool{}
	for index := uint64(0); index < blockCount; index++ {
		if ctx.Err() != nil {
			return nil, nil, false
		}
		hash, err := masterseed.ReadBlockHash(ctx, bytes.NewReader(seed), uint64(len(seed)), index)
		if err != nil {
			return nil, nil, false
		}
		hashHex := hash.Hex()
		ordered = append(ordered, hashHex)
		if seen[hashHex] {
			continue
		}
		raw, err := st.Load("staging/" + hashHex)
		if err != nil || len(raw) == 0 {
			continue
		}
		if _, err := masterseed.VerifyBlock(ctx, raw, hash); err != nil {
			continue
		}
		seen[hashHex] = true
		reused = append(reused, hashHex)
	}
	return ordered, reused, true
}

func (s *Server) handleRedownload(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}
	seed := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/downloads/"), "/redownload")
	if !validSeedHash(seed) || strings.Contains(seed, "/") {
		http.Error(writer, "invalid seed hash", http.StatusBadRequest)
		return
	}
	if s.options.ClientDir == "" {
		http.Error(writer, "client directory is not configured", http.StatusNotFound)
		return
	}
	var body struct {
		OutDir string `json:"outDir"`
	}
	if request.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024)).Decode(&body); err != nil {
			http.Error(writer, "invalid json", http.StatusBadRequest)
			return
		}
	}
	if s.options.StartDownload == nil {
		http.Error(writer, "download starter is not configured", http.StatusInternalServerError)
		return
	}
	// The whole check-create-switch sequence runs under the in-process mutex
	// and the seed file guard: a second request (or an interactive `km <seed>`)
	// either waits outside or fails the guard with 409 instead of racing the
	// pointer switch. The background job starts only after the switch, so it
	// re-acquires the guard itself and never deadlocks against this section.
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	// Reject shutdown races before any persistent mutation: startJob's own
	// check stays as defense, but by then a snapshot would already be
	// created and the pointer already switched.
	if s.closing {
		http.Error(writer, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	if job := s.jobStatus(seed); job != nil && !job.Done {
		http.Error(writer, "a download for this seed is currently active; stop it before redownloading", http.StatusConflict)
		return
	}
	guard, err := lock.Acquire(filepath.Join(clientSeedDir(s.options.ClientDir, seed), "scheduler", ".active.lock"))
	if err != nil {
		http.Error(writer, "a download for this seed is currently active; stop it before redownloading", http.StatusConflict)
		return
	}
	st, err := s.seedStore(seed)
	if err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	current, exists, err := host.CurrentExecID(st, seed)
	if err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		_ = guard.Release()
		http.Error(writer, "no execution exists for this seed; run `km <seed>` once first", http.StatusConflict)
		return
	}
	state, _, err := host.LoadExecSnapshot(st, seed, current)
	if err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	view := scheduler.ProjectView(state)
	if ok, reason := host.RedownloadEligibility(state, view); !ok {
		_ = guard.Release()
		http.Error(writer, "redownload is not allowed: "+reason, http.StatusConflict)
		return
	}
	// Build the new execution with the same kernel config as the current one,
	// so limits and pricing policy do not silently change across executions.
	sched, err := scheduler.New(state.Config)
	if err != nil {
		_ = guard.Release()
		http.Error(writer, "current execution config is invalid: "+err.Error(), http.StatusInternalServerError)
		return
	}
	create := scheduler.CreateTaskInput{SeedHash: seed}
	if ordered, reused, ok := reuseFromStaging(request.Context(), st, seed); ok && len(ordered) > 0 {
		create.Resume = &scheduler.CreateResume{BlockHashes: ordered, ReusedBlockHashes: reused}
	}
	newExec, err := host.CreateExecution(st, sched, seed, create)
	if err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := host.SwitchCurrent(st, seed, newExec); err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = guard.Release()
	outDir := strings.TrimSpace(body.OutDir)
	if outDir == "" {
		var dirErr error
		outDir, dirErr = os.Getwd()
		if dirErr != nil {
			http.Error(writer, "cannot determine output directory: "+dirErr.Error(), http.StatusInternalServerError)
			return
		}
	}
	job, ok := s.startJob(seed, newExec, outDir)
	if !ok {
		http.Error(writer, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	writeJSON(writer, map[string]any{
		"seedHash": seed, "execId": newExec, "previous": current,
		"started": true, "job": job,
		"note": "the new execution is now driving in the background; the old execution " + current + " stays inspectable and can be made current again",
	})
}

func (s *Server) handleSwitch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}
	seed := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/downloads/"), "/switch")
	if !validSeedHash(seed) || strings.Contains(seed, "/") {
		http.Error(writer, "invalid seed hash", http.StatusBadRequest)
		return
	}
	if s.options.ClientDir == "" {
		http.Error(writer, "client directory is not configured", http.StatusNotFound)
		return
	}
	var body struct {
		ExecID string `json:"execId"`
		// Start drives the switched-to execution in the background, so its
		// outstanding delivery, close and refund work actually proceeds.
		Start  bool   `json:"start"`
		OutDir string `json:"outDir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024)).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}
	if !host.ValidateExecID(body.ExecID) {
		http.Error(writer, "invalid exec id", http.StatusBadRequest)
		return
	}
	if body.Start && s.options.StartDownload == nil {
		http.Error(writer, "download starter is not configured", http.StatusInternalServerError)
		return
	}
	// Same single-flight discipline as redownload: mutex plus file guard
	// across the check-and-switch, job start only afterwards.
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	// Reject shutdown races before any persistent mutation: startJob's own
	// check stays as defense, but by then a snapshot would already be
	// created and the pointer already switched.
	if s.closing {
		http.Error(writer, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	if job := s.jobStatus(seed); job != nil && !job.Done {
		http.Error(writer, "a download for this seed is currently active; stop it before switching", http.StatusConflict)
		return
	}
	guard, err := lock.Acquire(filepath.Join(clientSeedDir(s.options.ClientDir, seed), "scheduler", ".active.lock"))
	if err != nil {
		http.Error(writer, "a download for this seed is currently active; stop it before switching", http.StatusConflict)
		return
	}
	st, err := s.seedStore(seed)
	if err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if _, _, err := host.LoadExecSnapshot(st, seed, body.ExecID); err != nil {
		_ = guard.Release()
		http.Error(writer, "execution has no snapshot: "+err.Error(), http.StatusNotFound)
		return
	}
	previous, _, _ := host.CurrentExecID(st, seed)
	if err := host.SwitchCurrent(st, seed, body.ExecID); err != nil {
		_ = guard.Release()
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = guard.Release()
	// Switching back to an old execution is the handling entry for its
	// outstanding delivery, close and refund work. With start=true the admin
	// drives that execution in the background right away.
	if !body.Start {
		writeJSON(writer, map[string]any{
			"seedHash": seed, "current": body.ExecID, "previous": previous,
			"started": false,
			"next":    "run `km " + seed + "` to resume execution " + body.ExecID,
		})
		return
	}
	outDir := strings.TrimSpace(body.OutDir)
	if outDir == "" {
		var dirErr error
		outDir, dirErr = os.Getwd()
		if dirErr != nil {
			http.Error(writer, "cannot determine output directory: "+dirErr.Error(), http.StatusInternalServerError)
			return
		}
	}
	job, ok := s.startJob(seed, body.ExecID, outDir)
	if !ok {
		http.Error(writer, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	writeJSON(writer, map[string]any{
		"seedHash": seed, "current": body.ExecID, "previous": previous,
		"started": true, "job": job,
	})
}

// handleDownloadJob reports the background download job for a seed, if any.
func (s *Server) handleDownloadJob(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}
	seed := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/downloads/"), "/job")
	if !validSeedHash(seed) || strings.Contains(seed, "/") {
		http.Error(writer, "invalid seed hash", http.StatusBadRequest)
		return
	}
	if job := s.jobStatus(seed); job != nil {
		writeJSON(writer, map[string]any{"job": job})
		return
	}
	writeJSON(writer, map[string]any{"job": nil})
}
