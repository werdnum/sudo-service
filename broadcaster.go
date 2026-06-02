package main

import (
	"sync"
)

// Event is what gets pushed over SSE on /requests/{uid}/events.
type Event struct {
	Type            string `json:"type"`
	Phase           string `json:"phase,omitempty"`
	ApprovedBy      string `json:"approvedBy,omitempty"`
	DeniedBy        string `json:"deniedBy,omitempty"`
	DenialReason    string `json:"denialReason,omitempty"`
	ExitCode        *int32 `json:"exitCode,omitempty"`
	OutputSecretRef string `json:"outputSecretRef,omitempty"`
}

// Broadcaster is a simple pub/sub keyed by SudoRequest UID. SSE handlers Subscribe;
// the reconciler Publishes on phase transitions.
//
// Subscribers MUST drain or close their channel; we drop on full to avoid blocking
// the reconciler.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[string]map[chan Event]struct{})}
}

func (b *Broadcaster) Subscribe(uid string) (<-chan Event, func()) {
	ch := make(chan Event, 8)
	b.mu.Lock()
	if b.subs[uid] == nil {
		b.subs[uid] = make(map[chan Event]struct{})
	}
	b.subs[uid][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if m, ok := b.subs[uid]; ok {
			delete(m, ch)
			if len(m) == 0 {
				delete(b.subs, uid)
			}
		}
		b.mu.Unlock()
		// Deliberately do NOT close(ch). Publish snapshots channels under
		// the lock, then sends after unlocking — closing here would race
		// with that send and panic with "send on closed channel". The
		// channel just becomes garbage once both the subscriber map entry
		// and the subscriber's goroutine drop their references; the
		// subscriber terminates on its request context instead of on
		// channel close.
	}
}

func (b *Broadcaster) Publish(uid string, e Event) {
	b.mu.Lock()
	chans := make([]chan Event, 0, len(b.subs[uid]))
	for ch := range b.subs[uid] {
		chans = append(chans, ch)
	}
	b.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- e:
		default:
			// Subscriber is slow; drop event rather than block.
		}
	}
}
