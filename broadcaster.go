package main

import (
	"sync"
)

// Event is what gets pushed over SSE on /requests/{uid}/events.
type Event struct {
	Type                string `json:"type"`
	UID                 string `json:"uid,omitempty"`
	Phase               string `json:"phase,omitempty"`
	Requester           string `json:"requester,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Command             string `json:"command,omitempty"`
	CreatedAt           string `json:"createdAt,omitempty"`
	RetryOfUID          string `json:"retryOfUID,omitempty"`
	ApprovedBy          string `json:"approvedBy,omitempty"`
	DeniedBy            string `json:"deniedBy,omitempty"`
	DenialReason        string `json:"denialReason,omitempty"`
	FailureReason       string `json:"failureReason,omitempty"`
	ExitCode            *int32 `json:"exitCode,omitempty"`
	OutputSecretRef     string `json:"outputSecretRef,omitempty"`
	OutputCaptureState  string `json:"outputCaptureState,omitempty"`
	OutputDeliveryState string `json:"outputDeliveryState,omitempty"`
	OutputFailureReason string `json:"outputFailureReason,omitempty"`
	OutputTotalBytes    *int64 `json:"outputTotalBytes,omitempty"`
	OutputRetainedBytes *int64 `json:"outputRetainedBytes,omitempty"`
	OutputSHA256        string `json:"outputSHA256,omitempty"`
}

// Broadcaster is a simple pub/sub keyed by SudoRequest UID. SSE handlers Subscribe;
// the reconciler Publishes on phase transitions.
//
// Subscribers MUST drain or close their channel; we drop on full to avoid blocking
// the reconciler.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
	all  map[chan Event]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs: make(map[string]map[chan Event]struct{}),
		all:  make(map[chan Event]struct{}),
	}
}

func (b *Broadcaster) SubscribeAll() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.all[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.all, ch)
		b.mu.Unlock()
	}
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
	e.UID = uid
	b.mu.Lock()
	chans := make([]chan Event, 0, len(b.subs[uid])+len(b.all))
	for ch := range b.subs[uid] {
		chans = append(chans, ch)
	}
	for ch := range b.all {
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
