package sysadmin

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("another Docker operation already owns a target or this request key was used for a different request")

type Store struct {
	db *sql.DB
}

func OpenStore(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ensureSchema keeps standalone tests and fresh starts working even when run
// outside COGNIX[WS]'s migration runner; the statements match 001_sysadmin.sql.
func (s *Store) ensureSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS docker_locks (endpoint TEXT NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, owner TEXT NOT NULL, PRIMARY KEY (endpoint, kind, target))`,
		`CREATE TABLE IF NOT EXISTS docker_operations (id TEXT PRIMARY KEY, request_key TEXT NOT NULL UNIQUE, endpoint TEXT NOT NULL, container_id TEXT NOT NULL, container_name TEXT NOT NULL, action TEXT NOT NULL, actor TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS idx_docker_ops_created ON docker_operations (created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS docker_plans (id TEXT PRIMARY KEY, created_at TEXT NOT NULL, expires_at TEXT NOT NULL, payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS docker_jobs (id TEXT PRIMARY KEY, request_key TEXT NOT NULL UNIQUE, plan_id TEXT NOT NULL UNIQUE, state TEXT NOT NULL, created_at TEXT NOT NULL, payload TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_docker_jobs_created ON docker_jobs (created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS docker_monitors (endpoint TEXT NOT NULL, project TEXT NOT NULL, revision INTEGER NOT NULL, payload TEXT NOT NULL, PRIMARY KEY (endpoint, project))`,
		`CREATE TABLE IF NOT EXISTS docker_incidents (id TEXT PRIMARY KEY, endpoint TEXT NOT NULL, project TEXT NOT NULL, signal TEXT NOT NULL, state TEXT NOT NULL, updated_at TEXT NOT NULL, payload TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_docker_incidents_proj ON docker_incidents (endpoint, project, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS docker_compose_projects (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL DEFAULT '', project TEXT NOT NULL UNIQUE, work_dir TEXT NOT NULL, files_json TEXT NOT NULL, profiles_json TEXT NOT NULL DEFAULT '[]', updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS docker_confirmations (id TEXT PRIMARY KEY, revision TEXT NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL, message TEXT NOT NULL, confirm_label TEXT NOT NULL, danger INTEGER NOT NULL DEFAULT 0, payload TEXT NOT NULL, expires_at TEXT NOT NULL)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

func newRevision(seed string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	sum := sha256.Sum256(append([]byte(seed), b[:]...))
	return hex.EncodeToString(sum[:])
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func rollback(tx *sql.Tx) { _ = tx.Rollback() }

func reserveTarget(tx *sql.Tx, endpoint, kind, target, owner string) error {
	_, err := tx.Exec(`INSERT INTO docker_locks (endpoint, kind, target, owner) VALUES (?, ?, ?, ?)`, endpoint, kind, target, owner)
	if err != nil {
		var existing string
		if e := tx.QueryRow(`SELECT owner FROM docker_locks WHERE endpoint = ? AND kind = ? AND target = ?`, endpoint, kind, target).Scan(&existing); e == nil {
			return ErrConflict
		}
	}
	return err
}

// ---------- Operations ----------

type Operation struct {
	ID            string `json:"id"`
	RequestKey    string `json:"requestKey"`
	Endpoint      string `json:"endpoint"`
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	Action        string `json:"action"`
	Actor         string `json:"actor"`
	AgentID       string `json:"agentId,omitempty"`
	State         string `json:"state"`
	Message       string `json:"message,omitempty"`
	CreatedAt     string `json:"createdAt"`
	FinishedAt    string `json:"finishedAt,omitempty"`
}

const opCols = `id, request_key, endpoint, container_id, container_name, action, actor, agent_id, state, message, created_at, finished_at`

func scanOp(row interface{ Scan(...any) error }) (Operation, error) {
	var op Operation
	err := row.Scan(&op.ID, &op.RequestKey, &op.Endpoint, &op.ContainerID, &op.ContainerName, &op.Action, &op.Actor, &op.AgentID, &op.State, &op.Message, &op.CreatedAt, &op.FinishedAt)
	return op, err
}

func (s *Store) Operation(id string) (Operation, error) {
	return scanOp(s.db.QueryRow(`SELECT `+opCols+` FROM docker_operations WHERE id = ?`, id))
}

func (s *Store) Operations(limit int) ([]Operation, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+opCols+` FROM docker_operations WHERE state = 'running' OR id IN (SELECT id FROM docker_operations ORDER BY created_at DESC LIMIT ?) ORDER BY created_at DESC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Operation{}
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (s *Store) BeginOperation(op Operation) (Operation, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return op, false, err
	}
	defer rollback(tx)
	old, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM docker_operations WHERE request_key = ?`, op.RequestKey))
	if err == nil {
		if old.Endpoint != op.Endpoint || old.ContainerID != op.ContainerID || old.Action != op.Action || old.AgentID != op.AgentID {
			return old, false, ErrConflict
		}
		return old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return op, false, err
	}
	op.ID, op.State, op.CreatedAt = newID("op"), "running", nowUTC()
	if err = reserveTarget(tx, op.Endpoint, "container", op.ContainerID, op.ID); err != nil {
		return op, false, err
	}
	if _, err = tx.Exec(`INSERT INTO docker_operations (`+opCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		op.ID, op.RequestKey, op.Endpoint, op.ContainerID, op.ContainerName, op.Action, op.Actor, op.AgentID, op.State, "", op.CreatedAt, ""); err != nil {
		return op, false, err
	}
	return op, true, tx.Commit()
}

func (s *Store) FinishOperation(id, state, message string) error {
	if state != "succeeded" && state != "failed" && state != "unknown" {
		return fmt.Errorf("invalid outcome %q", state)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	op, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM docker_operations WHERE id = ?`, id))
	if err != nil {
		return err
	}
	if op.State != "running" {
		return nil
	}
	fin := nowUTC()
	if _, err = tx.Exec(`DELETE FROM docker_locks WHERE owner = ?`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE docker_operations SET state = ?, message = ?, finished_at = ? WHERE id = ? AND state = 'running'`, state, message, fin, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- Plans & Jobs ----------

type Step struct {
	Kind          string `json:"kind"` // container | image | network | volume | compose | verify
	Target        string `json:"target"`
	Name          string `json:"name"`
	Action        string `json:"action"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	MemoryPercent int    `json:"memoryPercent,omitempty"`
	Condition     string `json:"condition,omitempty"`
	State         string `json:"state"` // queued | running | succeeded | skipped | failed | unknown
	Message       string `json:"message,omitempty"`
}

type Plan struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"` // project | resource | procedure | compose
	Title       string          `json:"title"`
	Impact      string          `json:"impact"`
	Endpoint    string          `json:"endpoint"`
	Project     string          `json:"project"`
	WorkspaceID string          `json:"workspaceId,omitempty"`
	Actor       string          `json:"actor"`
	AgentID     string          `json:"agentId,omitempty"`
	CreatedAt   string          `json:"createdAt"`
	ExpiresAt   string          `json:"expiresAt"`
	Fingerprint string          `json:"fingerprint"`
	Input       json.RawMessage `json:"input"`
	Steps       []Step          `json:"steps"`
	InboxID     string          `json:"inboxId,omitempty"`
}

type Job struct {
	ID              string `json:"id"`
	RequestKey      string `json:"requestKey"`
	PlanID          string `json:"planId"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	Endpoint        string `json:"endpoint"`
	Project         string `json:"project"`
	Actor           string `json:"actor"`
	AgentID         string `json:"agentId,omitempty"`
	ApprovedBy      string `json:"approvedBy"`
	ApproverAgentID string `json:"approverAgentId,omitempty"`
	State           string `json:"state"`
	Message         string `json:"message,omitempty"`
	Steps           []Step `json:"steps"`
	CreatedAt       string `json:"createdAt"`
	FinishedAt      string `json:"finishedAt,omitempty"`
}

func scanJSON[T any](row interface{ Scan(...any) error }) (T, error) {
	var v T
	var raw string
	if err := row.Scan(&raw); err != nil {
		return v, err
	}
	return v, json.Unmarshal([]byte(raw), &v)
}

func (s *Store) CreatePlan(p Plan) (Plan, error) {
	p.ID, p.CreatedAt = newID("plan"), nowUTC()
	p.ExpiresAt = time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	_, err = s.db.Exec(`INSERT INTO docker_plans (id, created_at, expires_at, payload) VALUES (?, ?, ?, ?)`, p.ID, p.CreatedAt, p.ExpiresAt, string(raw))
	return p, err
}

func (s *Store) Plan(id string) (Plan, error) {
	return scanJSON[Plan](s.db.QueryRow(`SELECT payload FROM docker_plans WHERE id = ?`, id))
}

func (s *Store) SetPlanInboxID(id, inboxID string) (Plan, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Plan{}, err
	}
	defer rollback(tx)
	p, err := scanJSON[Plan](tx.QueryRow(`SELECT payload FROM docker_plans WHERE id = ?`, id))
	if err != nil {
		return Plan{}, err
	}
	if p.InboxID != "" {
		return p, nil
	}
	p.InboxID = inboxID
	raw, _ := json.Marshal(p)
	if _, err = tx.Exec(`UPDATE docker_plans SET payload = ? WHERE id = ?`, string(raw), id); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

// PendingPlans returns unexecuted, unexpired plans newest first so the page
// surfaces agent plans waiting for the owner's approval.
func (s *Store) PendingPlans() ([]Plan, error) {
	now := nowUTC()
	rows, err := s.db.Query(`SELECT payload FROM docker_plans WHERE expires_at > ? AND id NOT IN (SELECT plan_id FROM docker_jobs) ORDER BY created_at DESC LIMIT 25`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanJSON[Plan](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Job(id string) (Job, error) {
	return scanJSON[Job](s.db.QueryRow(`SELECT payload FROM docker_jobs WHERE id = ?`, id))
}

func (s *Store) Jobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT payload FROM docker_jobs WHERE state = 'running' OR id IN (SELECT id FROM docker_jobs ORDER BY created_at DESC LIMIT 50) ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJSON[Job](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) BeginJob(j Job) (Job, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return j, false, err
	}
	defer rollback(tx)
	old, err := scanJSON[Job](tx.QueryRow(`SELECT payload FROM docker_jobs WHERE request_key = ?`, j.RequestKey))
	if err == nil {
		if old.PlanID != j.PlanID || old.ApproverAgentID != j.ApproverAgentID {
			return old, false, ErrConflict
		}
		return old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return j, false, err
	}
	var used int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM docker_jobs WHERE plan_id = ?`, j.PlanID).Scan(&used); err != nil {
		return j, false, err
	}
	if used > 0 {
		return j, false, ErrConflict
	}
	j.ID, j.State, j.CreatedAt = newID("job"), "running", nowUTC()
	j.Message = "Execution in progress."
	reserved := map[string]bool{}
	for _, step := range j.Steps {
		if step.Kind == "verify" {
			continue
		}
		key := step.Kind + ":" + step.Target
		if reserved[key] {
			continue
		}
		if err = reserveTarget(tx, j.Endpoint, step.Kind, step.Target, j.ID); err != nil {
			return j, false, err
		}
		reserved[key] = true
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return j, false, err
	}
	if _, err = tx.Exec(`INSERT INTO docker_jobs (id, request_key, plan_id, state, created_at, payload) VALUES (?, ?, ?, ?, ?, ?)`, j.ID, j.RequestKey, j.PlanID, j.State, j.CreatedAt, string(raw)); err != nil {
		return j, false, err
	}
	return j, true, tx.Commit()
}

func (s *Store) UpdateJob(j Job) error {
	if j.State != "running" && j.State != "succeeded" && j.State != "partial" && j.State != "failed" && j.State != "unknown" {
		return fmt.Errorf("invalid job outcome %q", j.State)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	old, err := scanJSON[Job](tx.QueryRow(`SELECT payload FROM docker_jobs WHERE id = ?`, j.ID))
	if err != nil {
		return err
	}
	if old.State != "running" {
		return nil
	}
	if j.State != "running" {
		j.FinishedAt = nowUTC()
		if _, err = tx.Exec(`DELETE FROM docker_locks WHERE owner = ?`, j.ID); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE docker_jobs SET state = ?, payload = ? WHERE id = ? AND state = 'running'`, j.State, string(raw), j.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// Recover marks any interrupted operation or job unknown without replaying
// host mutations, and releases all leftover locks.
func (s *Store) Recover() error {
	rows, err := s.db.Query(`SELECT id FROM docker_operations WHERE state = 'running'`)
	if err != nil {
		return err
	}
	var opIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		opIDs = append(opIDs, id)
	}
	_ = rows.Close()
	for _, id := range opIDs {
		if err := s.FinishOperation(id, "unknown", "The Docker extension restarted before the outcome was verified. Inspect the container before retrying."); err != nil {
			return err
		}
	}

	jobs, err := s.Jobs()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State != "running" {
			continue
		}
		for i := range j.Steps {
			if j.Steps[i].State == "running" || j.Steps[i].State == "queued" {
				j.Steps[i].State = "unknown"
				j.Steps[i].Message = "Interrupted by extension restart; not replayed."
			}
		}
		j.State = "unknown"
		j.Message = "Interrupted by extension restart. Inspect targets before retrying."
		if err := s.UpdateJob(j); err != nil {
			return err
		}
	}
	_, _ = s.db.Exec(`DELETE FROM docker_confirmations WHERE expires_at <= ?`, nowUTC())
	return nil
}

// ---------- Confirmations (picode.use("confirm") door) ----------

type ConfirmationView struct {
	Title        string `json:"title"`
	Message      string `json:"message"`
	ConfirmLabel string `json:"confirmLabel"`
	Danger       bool   `json:"danger"`
	Revision     string `json:"revision"`
}

type ConfirmationRecord struct {
	ID           string
	Revision     string
	Kind         string
	Confirmation ConfirmationView
	Payload      json.RawMessage
	ExpiresAt    string
}

func (s *Store) CreateConfirmation(kind, title, message, confirmLabel string, danger bool, payload any) (ConfirmationRecord, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ConfirmationRecord{}, err
	}
	id := newID("cfm")
	rev := newRevision(id + string(raw))
	exp := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	dInt := 0
	if danger {
		dInt = 1
	}
	if _, err := s.db.Exec(`INSERT INTO docker_confirmations (id, revision, kind, title, message, confirm_label, danger, payload, expires_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, rev, kind, title, message, confirmLabel, dInt, string(raw), exp); err != nil {
		return ConfirmationRecord{}, err
	}
	return ConfirmationRecord{
		ID: id, Revision: rev, Kind: kind, Payload: raw, ExpiresAt: exp,
		Confirmation: ConfirmationView{Title: title, Message: message, ConfirmLabel: confirmLabel, Danger: danger, Revision: rev},
	}, nil
}

func (s *Store) GetConfirmation(id string) (ConfirmationRecord, error) {
	var rec ConfirmationRecord
	var dInt int
	var raw string
	err := s.db.QueryRow(`SELECT id, revision, kind, title, message, confirm_label, danger, payload, expires_at FROM docker_confirmations WHERE id = ?`, id).
		Scan(&rec.ID, &rec.Revision, &rec.Kind, &rec.Confirmation.Title, &rec.Confirmation.Message, &rec.Confirmation.ConfirmLabel, &dInt, &raw, &rec.ExpiresAt)
	if err != nil {
		return rec, err
	}
	rec.Confirmation.Danger = dInt != 0
	rec.Confirmation.Revision = rec.Revision
	rec.Payload = json.RawMessage(raw)
	exp, err := time.Parse(time.RFC3339Nano, rec.ExpiresAt)
	if err != nil || !time.Now().Before(exp) {
		return rec, errors.New("confirmation expired")
	}
	return rec, nil
}

func (s *Store) ConsumeConfirmation(id, revision string) (ConfirmationRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ConfirmationRecord{}, err
	}
	defer rollback(tx)
	var rec ConfirmationRecord
	var dInt int
	var raw string
	err = tx.QueryRow(`SELECT id, revision, kind, title, message, confirm_label, danger, payload, expires_at FROM docker_confirmations WHERE id = ?`, id).
		Scan(&rec.ID, &rec.Revision, &rec.Kind, &rec.Confirmation.Title, &rec.Confirmation.Message, &rec.Confirmation.ConfirmLabel, &dInt, &raw, &rec.ExpiresAt)
	if err != nil {
		return rec, err
	}
	if rec.Revision != revision || len(revision) != 64 {
		return rec, ErrConflict
	}
	exp, err := time.Parse(time.RFC3339Nano, rec.ExpiresAt)
	if err != nil || !time.Now().Before(exp) {
		_, _ = tx.Exec(`DELETE FROM docker_confirmations WHERE id = ?`, id)
		_ = tx.Commit()
		return rec, errors.New("confirmation expired")
	}
	if _, err := tx.Exec(`DELETE FROM docker_confirmations WHERE id = ?`, id); err != nil {
		return rec, err
	}
	rec.Confirmation.Danger = dInt != 0
	rec.Confirmation.Revision = rec.Revision
	rec.Payload = json.RawMessage(raw)
	return rec, tx.Commit()
}

// ---------- Health Monitors & Incidents ----------

type Monitor struct {
	Endpoint        string          `json:"endpoint"`
	Project         string          `json:"project"`
	Enabled         bool            `json:"enabled"`
	IntervalSeconds int             `json:"intervalSeconds"`
	CPUPercent      int             `json:"cpuPercent"`
	MemoryPercent   int             `json:"memoryPercent"`
	BadSamples      int             `json:"badSamples"`
	RetentionDays   int             `json:"retentionDays"`
	Revision        int             `json:"revision"`
	Actor           string          `json:"actor"`
	UpdatedAt       string          `json:"updatedAt"`
	SampledAt       string          `json:"sampledAt,omitempty"`
	EvaluatedAt     string          `json:"evaluatedAt,omitempty"`
	Snapshot        json.RawMessage `json:"snapshot,omitempty"`
}

type Signal struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	State  string `json:"state"` // good | bad | unknown
	Detail string `json:"detail"`
}

type Incident struct {
	ID         string `json:"id"`
	Endpoint   string `json:"endpoint"`
	Project    string `json:"project"`
	Signal     string `json:"signal"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	State      string `json:"state"` // pending | open | resolved
	BadStreak  int    `json:"badStreak"`
	GoodStreak int    `json:"goodStreak"`
	Revision   int    `json:"revision"`
	CreatedAt  string `json:"createdAt"`
	OpenedAt   string `json:"openedAt,omitempty"`
	UpdatedAt  string `json:"updatedAt"`
	ResolvedAt string `json:"resolvedAt,omitempty"`
}

func DefaultMonitor(endpoint, project string) Monitor {
	return Monitor{Endpoint: endpoint, Project: project, IntervalSeconds: 60, CPUPercent: 90, MemoryPercent: 85, BadSamples: 3, RetentionDays: 7}
}

func (m Monitor) Validate() error {
	if m.Endpoint == "" || len(m.Project) > 256 ||
		(m.IntervalSeconds != 30 && m.IntervalSeconds != 60 && m.IntervalSeconds != 300) ||
		(m.CPUPercent != 80 && m.CPUPercent != 90 && m.CPUPercent != 200) ||
		(m.MemoryPercent != 80 && m.MemoryPercent != 85 && m.MemoryPercent != 95) ||
		(m.BadSamples != 2 && m.BadSamples != 3 && m.BadSamples != 5) ||
		(m.RetentionDays != 7 && m.RetentionDays != 30) {
		return fmt.Errorf("choose one of the supported monitoring intervals and thresholds")
	}
	return nil
}

func (s *Store) Monitor(endpoint, project string) (Monitor, error) {
	return scanJSON[Monitor](s.db.QueryRow(`SELECT payload FROM docker_monitors WHERE endpoint = ? AND project = ?`, endpoint, project))
}

func (s *Store) Monitors() ([]Monitor, error) {
	rows, err := s.db.Query(`SELECT payload FROM docker_monitors ORDER BY endpoint, project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Monitor{}
	for rows.Next() {
		m, err := scanJSON[Monitor](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SaveMonitor(m Monitor) (Monitor, error) {
	if err := m.Validate(); err != nil {
		return m, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return m, err
	}
	defer rollback(tx)
	var revision int
	if err = tx.QueryRow(`SELECT COALESCE(MAX(revision), 0) FROM docker_monitors WHERE endpoint = ? AND project = ?`, m.Endpoint, m.Project).Scan(&revision); err != nil {
		return m, err
	}
	if m.Revision != 0 && revision != m.Revision {
		return m, ErrConflict
	}
	if revision > 0 {
		if old, e := scanJSON[Monitor](tx.QueryRow(`SELECT payload FROM docker_monitors WHERE endpoint = ? AND project = ?`, m.Endpoint, m.Project)); e == nil {
			m.Snapshot, m.SampledAt = old.Snapshot, old.SampledAt
		}
	}
	if m.Enabled {
		var total int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM docker_monitors WHERE json_extract(payload, '$.enabled') = 1 AND NOT (endpoint = ? AND project = ?)`, m.Endpoint, m.Project).Scan(&total); err != nil {
			return m, err
		}
		if total >= 32 {
			return m, fmt.Errorf("up to 32 projects can be monitored concurrently")
		}
	}
	m.Revision, m.UpdatedAt, m.EvaluatedAt = revision+1, nowUTC(), ""
	raw, _ := json.Marshal(m)
	if _, err = tx.Exec(`INSERT INTO docker_monitors (endpoint, project, revision, payload) VALUES (?,?,?,?) ON CONFLICT(endpoint, project) DO UPDATE SET revision=excluded.revision, payload=excluded.payload`,
		m.Endpoint, m.Project, m.Revision, string(raw)); err != nil {
		return m, err
	}
	return m, tx.Commit()
}

func (s *Store) Incidents(endpoint, project string) ([]Incident, error) {
	rows, err := s.db.Query(`SELECT payload FROM docker_incidents WHERE endpoint = ? AND project = ? AND (state = 'open' OR id IN (SELECT id FROM docker_incidents WHERE endpoint = ? AND project = ? AND state != 'pending' ORDER BY updated_at DESC LIMIT 100)) ORDER BY updated_at DESC`, endpoint, project, endpoint, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		inc, err := scanJSON[Incident](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *Store) OpenIncidents() ([]Incident, error) {
	rows, err := s.db.Query(`SELECT payload FROM docker_incidents WHERE state = 'open' ORDER BY updated_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		inc, err := scanJSON[Incident](rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *Store) RecordHealth(endpoint, project string, revision int, snapshot json.RawMessage, signals []Signal, at time.Time) error {
	if !json.Valid(snapshot) || len(snapshot) > 1024*1024 {
		return fmt.Errorf("invalid or oversized health snapshot")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	m, err := scanJSON[Monitor](tx.QueryRow(`SELECT payload FROM docker_monitors WHERE endpoint = ? AND project = ?`, endpoint, project))
	if err != nil {
		return err
	}
	if m.Revision != revision {
		return ErrConflict
	}
	prevSample, _ := time.Parse(time.RFC3339Nano, m.SampledAt)
	if !prevSample.IsZero() && !at.After(prevSample) {
		return nil
	}
	m.Snapshot, m.SampledAt = snapshot, at.UTC().Format(time.RFC3339Nano)
	last, _ := time.Parse(time.RFC3339Nano, m.EvaluatedAt)
	if m.Enabled && (last.IsZero() || at.Sub(last) >= time.Duration(m.IntervalSeconds)*time.Second) {
		observed := map[string]Signal{}
		for _, sig := range signals {
			observed[sig.Key] = sig
		}
		rows, e := tx.Query(`SELECT payload FROM docker_incidents WHERE endpoint = ? AND project = ? AND state IN ('pending', 'open')`, endpoint, project)
		if e != nil {
			return e
		}
		active := map[string]Incident{}
		for rows.Next() {
			v, e := scanJSON[Incident](rows)
			if e != nil {
				_ = rows.Close()
				return e
			}
			active[v.Signal] = v
		}
		_ = rows.Close()
		for key, inc := range active {
			if _, ok := observed[key]; !ok {
				observed[key] = Signal{Key: key, Title: inc.Title, State: "unknown", Detail: "No current observation."}
			}
		}
		gap := !last.IsZero() && at.Sub(last) > time.Duration(2*m.IntervalSeconds)*time.Second
		for key, sig := range observed {
			inc, exists := active[key]
			if !exists && sig.State != "bad" {
				continue
			}
			if !exists {
				inc = Incident{ID: newID("inc"), Endpoint: endpoint, Project: project, Signal: key, State: "pending", CreatedAt: m.SampledAt}
			}
			if inc.Revision != m.Revision || gap {
				inc.BadStreak, inc.GoodStreak = 0, 0
			}
			inc.Revision, inc.Title, inc.Detail, inc.UpdatedAt = m.Revision, sig.Title, sig.Detail, m.SampledAt
			switch sig.State {
			case "bad":
				inc.BadStreak++
				inc.GoodStreak = 0
			case "good":
				inc.GoodStreak++
				inc.BadStreak = 0
			default:
				inc.BadStreak, inc.GoodStreak = 0, 0
			}
			if inc.State == "pending" && inc.BadStreak >= m.BadSamples {
				inc.State, inc.OpenedAt = "open", m.SampledAt
			}
			if inc.GoodStreak >= 2 {
				inc.State, inc.ResolvedAt = "resolved", m.SampledAt
			}
			raw, _ := json.Marshal(inc)
			if _, err = tx.Exec(`INSERT INTO docker_incidents (id, endpoint, project, signal, state, updated_at, payload) VALUES (?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state, updated_at=excluded.updated_at, payload=excluded.payload`,
				inc.ID, endpoint, project, key, inc.State, inc.UpdatedAt, string(raw)); err != nil {
				return err
			}
		}
		m.EvaluatedAt = m.SampledAt
	}
	cutoff := at.Add(-time.Duration(m.RetentionDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`DELETE FROM docker_incidents WHERE endpoint = ? AND project = ? AND state != 'open' AND updated_at < ?`, endpoint, project, cutoff); err != nil {
		return err
	}
	raw, _ := json.Marshal(m)
	if _, err = tx.Exec(`UPDATE docker_monitors SET payload = ? WHERE endpoint = ? AND project = ? AND revision = ?`, string(raw), endpoint, project, revision); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- Registered Compose Projects ----------

type ComposeRegistration struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspaceId,omitempty"`
	Project     string   `json:"project"`
	WorkDir     string   `json:"workDir"`
	Files       []string `json:"files"`
	Profiles    []string `json:"profiles,omitempty"`
	UpdatedAt   string   `json:"updatedAt"`
}

func (s *Store) SaveComposeProject(reg ComposeRegistration) (ComposeRegistration, error) {
	reg.Project = strings.TrimSpace(reg.Project)
	if reg.Project == "" || reg.WorkDir == "" || len(reg.Files) == 0 {
		return reg, errors.New("project name, folder and at least one Compose file are required")
	}
	if reg.ID == "" {
		reg.ID = newID("cmp")
	}
	reg.UpdatedAt = nowUTC()
	filesRaw, _ := json.Marshal(reg.Files)
	profRaw, _ := json.Marshal(reg.Profiles)
	_, err := s.db.Exec(`INSERT INTO docker_compose_projects (id, workspace_id, project, work_dir, files_json, profiles_json, updated_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(project) DO UPDATE SET workspace_id=excluded.workspace_id, work_dir=excluded.work_dir, files_json=excluded.files_json, profiles_json=excluded.profiles_json, updated_at=excluded.updated_at`,
		reg.ID, reg.WorkspaceID, reg.Project, reg.WorkDir, string(filesRaw), string(profRaw), reg.UpdatedAt)
	if err != nil {
		return reg, err
	}
	return s.ComposeProject(reg.Project)
}

func (s *Store) ComposeProject(project string) (ComposeRegistration, error) {
	var reg ComposeRegistration
	var fRaw, pRaw string
	err := s.db.QueryRow(`SELECT id, workspace_id, project, work_dir, files_json, profiles_json, updated_at FROM docker_compose_projects WHERE project = ?`, project).
		Scan(&reg.ID, &reg.WorkspaceID, &reg.Project, &reg.WorkDir, &fRaw, &pRaw, &reg.UpdatedAt)
	if err != nil {
		return reg, err
	}
	_ = json.Unmarshal([]byte(fRaw), &reg.Files)
	_ = json.Unmarshal([]byte(pRaw), &reg.Profiles)
	return reg, nil
}

func (s *Store) ComposeProjects() ([]ComposeRegistration, error) {
	rows, err := s.db.Query(`SELECT id, workspace_id, project, work_dir, files_json, profiles_json, updated_at FROM docker_compose_projects ORDER BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ComposeRegistration{}
	for rows.Next() {
		var reg ComposeRegistration
		var fRaw, pRaw string
		if err := rows.Scan(&reg.ID, &reg.WorkspaceID, &reg.Project, &reg.WorkDir, &fRaw, &pRaw, &reg.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(fRaw), &reg.Files)
		_ = json.Unmarshal([]byte(pRaw), &reg.Profiles)
		out = append(out, reg)
	}
	return out, rows.Err()
}
