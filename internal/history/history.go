// Package history is the daemon's durable run log: a bbolt store under
// ~/.agentcage that keeps one record per run so ps, logs, and trace survive the
// run ending and the daemon restarting. It is trusted-kernel code on the host.
// The agent never writes here; only the daemon does, keyed by the run id it
// assigned, so a record can never be forged by a cage.
//
// bbolt is the same embedded store containerd uses for its metadata: pure Go,
// single file, crash-safe, already in the dependency tree. Run history is
// key-value (the run id is the key), which is all ps/logs/trace need.
package history

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/okedeji/agentcage/internal/env"
)

// dbName is the history file under the agentcage home dir.
const dbName = "history.db"

// runsBucket holds one JSON Record per run, keyed by run id.
var runsBucket = []byte("runs")

// openTimeout bounds how long Open waits for the file lock. The socket guard
// already keeps a second daemon from starting, so contention here means a stale
// lock or a misconfiguration; failing fast beats hanging the daemon's boot.
const openTimeout = 2 * time.Second

// Run statuses. A run is running until it ends; the terminal states distinguish
// a clean finish from a failure, a budget cutoff, an operator stop, and a daemon
// that died under the run (reconciled to crashed at the next startup).
const (
	StatusRunning    = "running"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusOverBudget = "over_budget"
	StatusStopped    = "stopped"
	StatusCrashed    = "crashed"
)

// Record is one run's durable entry. Cost and budget are micro-USD integers, the
// same unit the LLM gateway meters in, so the history never rounds the meter.
// TraceJSON holds the run's serialized trace, what `agentcage trace` renders;
// it is empty for a run that made no LLM call.
type Record struct {
	RunID          string    `json:"run_id"`
	Ref            string    `json:"ref"`
	Status         string    `json:"status"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
	CostMicroUSD   int64     `json:"cost_micro_usd,omitempty"`
	BudgetMicroUSD int64     `json:"budget_micro_usd,omitempty"`
	Error          string    `json:"error,omitempty"`
	TraceJSON      string    `json:"trace_json,omitempty"`
}

// Store is the open history database.
type Store struct {
	db *bbolt.DB
}

// DefaultPath is ~/.agentcage/history.db, honoring AGENTCAGE_HOME.
func DefaultPath() (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dbName), nil
}

// Open opens the history store at path, creating the file and the runs bucket if
// they do not exist.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("opening run history %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(runsBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing run history: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Put writes a record, overwriting any prior record for the same run id. A run
// is written once at boot as running and again at its terminal state, so the
// last write wins.
func (s *Store) Put(r Record) error {
	buf, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding run record %s: %w", r.RunID, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(runsBucket).Put([]byte(r.RunID), buf)
	})
}

// Get returns the record for a run id and whether one exists.
func (s *Store) Get(runID string) (Record, bool, error) {
	var (
		rec   Record
		found bool
	)
	err := s.db.View(func(tx *bbolt.Tx) error {
		buf := tx.Bucket(runsBucket).Get([]byte(runID))
		if buf == nil {
			return nil
		}
		found = true
		return json.Unmarshal(buf, &rec)
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("reading run record %s: %w", runID, err)
	}
	return rec, found, nil
}

// List returns every record, newest run first.
func (s *Store) List() ([]Record, error) {
	var out []Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(runsBucket).ForEach(func(_, buf []byte) error {
			var rec Record
			if err := json.Unmarshal(buf, &rec); err != nil {
				return err
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing run history: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// ReconcileRunning rewrites every record still marked running to crashed, since a
// running record at daemon startup is one whose daemon died without finishing it.
// It is the history analogue of the orphan container sweep: a fresh daemon owns
// no prior run, so any run still "in progress" on disk is a casualty of the
// crash. Returns the number of records reconciled.
func (s *Store) ReconcileRunning(at time.Time) (int, error) {
	var n int
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(runsBucket)
		var stale []Record
		if err := b.ForEach(func(_, buf []byte) error {
			var rec Record
			if err := json.Unmarshal(buf, &rec); err != nil {
				return err
			}
			if rec.Status == StatusRunning {
				rec.Status = StatusCrashed
				rec.EndedAt = at
				stale = append(stale, rec)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, rec := range stale {
			buf, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(rec.RunID), buf); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("reconciling crashed runs: %w", err)
	}
	return n, nil
}
