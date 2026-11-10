// Package events implements an in-memory per-task event stream. The workflow
// publishes events (step lifecycle, streamed reasoning/content, artifacts,
// simulator results) and the HTTP/SSE control plane replays them to the CLI.
package events

import (
	"context"
	"sync"
	"time"
)

// Kind is an event type.
type Kind string

const (
	KindTaskStart Kind = "task_start"
	KindStepStart Kind = "step_start"
	KindStepEnd   Kind = "step_end"
	KindReasoning Kind = "reasoning"
	KindContent   Kind = "content"
	KindArtifact  Kind = "artifact"
	KindSimulator Kind = "simulator"
	KindInfo      Kind = "info"
	KindDone      Kind = "done"
)

// Event is one item on a task's event stream.
type Event struct {
	Seq    int64     `json:"seq"`
	TaskID string    `json:"task_id"`
	Kind   Kind      `json:"kind"`
	Step   string    `json:"step,omitempty"`
	Status string    `json:"status,omitempty"`
	Text   string    `json:"text,omitempty"`
	Time   time.Time `json:"time"`
}

type taskStream struct {
	events []Event
	done   bool
	signal chan struct{}
}

// Hub keeps per-task event streams in memory.
type Hub struct {
	mu    sync.Mutex
	tasks map[string]*taskStream
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{tasks: map[string]*taskStream{}}
}

func (h *Hub) stream(taskID string) *taskStream {
	stream := h.tasks[taskID]
	if stream == nil {
		stream = &taskStream{signal: make(chan struct{})}
		h.tasks[taskID] = stream
	}
	return stream
}

// Publish appends an event and wakes all waiters.
func (h *Hub) Publish(event Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	stream := h.stream(event.TaskID)
	event.Seq = int64(len(stream.events))
	event.Time = time.Now().UTC()
	stream.events = append(stream.events, event)
	if event.Kind == KindDone {
		stream.done = true
	}
	close(stream.signal)
	stream.signal = make(chan struct{})
	h.mu.Unlock()
}

// Wait returns buffered events from offset onward. It blocks until at least one
// event is available, the stream is finished, or ctx is cancelled.
func (h *Hub) Wait(ctx context.Context, taskID string, offset int) ([]Event, bool, error) {
	if h == nil {
		return nil, true, nil
	}
	for {
		h.mu.Lock()
		stream := h.stream(taskID)
		if offset < len(stream.events) || stream.done {
			batch := append([]Event(nil), stream.events[offset:]...)
			done := stream.done
			h.mu.Unlock()
			return batch, done, nil
		}
		signal := stream.signal
		h.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-signal:
		}
	}
}

// Emitter publishes events for a single task.
type Emitter struct {
	hub    *Hub
	taskID string
}

// Emitter returns an emitter bound to taskID.
func (h *Hub) Emitter(taskID string) *Emitter {
	if h == nil {
		return nil
	}
	return &Emitter{hub: h, taskID: taskID}
}

// Emit publishes one event.
func (e *Emitter) Emit(kind Kind, step, status, text string) {
	if e == nil || e.hub == nil {
		return
	}
	e.hub.Publish(Event{TaskID: e.taskID, Kind: kind, Step: step, Status: status, Text: text})
}

func (e *Emitter) TaskStart(prompt string)              { e.Emit(KindTaskStart, "", "running", prompt) }
func (e *Emitter) StepStart(step string)                { e.Emit(KindStepStart, step, "running", "") }
func (e *Emitter) StepEnd(step, status, summary string) { e.Emit(KindStepEnd, step, status, summary) }
func (e *Emitter) Reasoning(text string)                { e.Emit(KindReasoning, "", "", text) }
func (e *Emitter) Content(text string)                  { e.Emit(KindContent, "", "", text) }
func (e *Emitter) Artifact(path string)                 { e.Emit(KindArtifact, "", "", path) }
func (e *Emitter) Simulator(text string)                { e.Emit(KindSimulator, "simulator", "", text) }
func (e *Emitter) Info(text string)                     { e.Emit(KindInfo, "", "", text) }
func (e *Emitter) Done(status, summary string)          { e.Emit(KindDone, "", status, summary) }

type emitterKey struct{}

// WithEmitter attaches an emitter to ctx so the LLM client can publish streamed
// reasoning and content.
func WithEmitter(ctx context.Context, emitter *Emitter) context.Context {
	if emitter == nil {
		return ctx
	}
	return context.WithValue(ctx, emitterKey{}, emitter)
}

// EmitterFrom returns the emitter attached to ctx, if any.
func EmitterFrom(ctx context.Context) *Emitter {
	emitter, _ := ctx.Value(emitterKey{}).(*Emitter)
	return emitter
}
