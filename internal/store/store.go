package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Step struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

type Artifact struct {
	Path string `json:"path"`
	Role string `json:"role,omitempty"`
}

type Task struct {
	ID            string           `json:"id"`
	Prompt        string           `json:"prompt"`
	Workspace     string           `json:"workspace"`
	SourceMode    string           `json:"source_mode,omitempty"`
	DutTop        string           `json:"dut_top,omitempty"`
	Status        string           `json:"status"`
	Stage         string           `json:"stage"`
	Summary       string           `json:"summary,omitempty"`
	ContractJSON  string           `json:"contract_json,omitempty"`
	Iteration     int              `json:"iteration"`
	MaxIterations int              `json:"max_iterations"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Steps         []Step           `json:"steps"`
	Artifacts     []Artifact       `json:"artifacts"`
	Issues        []map[string]any `json:"issues,omitempty"`
}

type Store struct {
	mu    sync.RWMutex
	path  string
	tasks map[string]*Task
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path, tasks: make(map[string]*Task)}
	data, err := os.ReadFile(path)
	if err == nil {
		var items []*Task
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("read task store: %w", err)
		}
		for _, task := range items {
			if task.Status == "running" || task.Status == "queued" {
				task.Status = "interrupted"
				task.Stage = "restart_required"
			}
			s.tasks[task.ID] = task
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) Save(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyTask, err := clone(task)
	if err != nil {
		return err
	}
	copyTask.UpdatedAt = time.Now().UTC()
	s.tasks[task.ID] = copyTask
	return s.persistLocked()
}

func (s *Store) Get(id string) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	copyTask, err := clone(task)
	if err != nil {
		return nil, false
	}
	return copyTask, true
}

func (s *Store) List(limit int) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]*Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		copyTask, err := clone(task)
		if err == nil {
			items = append(items, copyTask)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) persistLocked() error {
	items := make([]*Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		items = append(items, task)
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func clone(task *Task) (*Task, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	var copyTask Task
	if err := json.Unmarshal(data, &copyTask); err != nil {
		return nil, err
	}
	return &copyTask, nil
}
