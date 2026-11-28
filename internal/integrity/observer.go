package integrity

import (
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// Observer receives the result of a completed (committed) run. It is called
// exactly once after COMMIT, never at run start, and never for a locked,
// canceled, failed-before-commit, or panicked attempt. Implementations must not
// block; the row is the source of truth even if notification is skipped or
// panics.
type Observer interface {
	IntegrityRunCompleted(passed bool)
}

// tieoutEvent is one buffered observer callback. Exactly one field is set.
type tieoutEvent struct {
	computed *tieout.Result
	failed   string
}

// tieoutBuffer records tie-out observations during the run and replays them to
// the real observer only after COMMIT. A canceled or rolled-back run therefore
// emits no tie-out gauge value or failure code.
type tieoutBuffer struct {
	target tieout.Observer
	events []tieoutEvent
}

func newTieoutBuffer(target tieout.Observer) *tieoutBuffer {
	if target == nil {
		return nil
	}
	return &tieoutBuffer{target: target}
}

// TieoutComputed implements tieout.Observer.
func (b *tieoutBuffer) TieoutComputed(result tieout.Result) {
	if b == nil {
		return
	}
	computed := result
	b.events = append(b.events, tieoutEvent{computed: &computed})
}

// TieoutFailed implements tieout.Observer.
func (b *tieoutBuffer) TieoutFailed(code string) {
	if b == nil {
		return
	}
	b.events = append(b.events, tieoutEvent{failed: code})
}

// flush replays buffered observations in their original order. A panic from the
// target observer is swallowed: the persisted run row remains authoritative.
func (b *tieoutBuffer) flush() {
	if b == nil || b.target == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	for _, event := range b.events {
		if event.computed != nil {
			b.target.TieoutComputed(*event.computed)
			continue
		}
		b.target.TieoutFailed(event.failed)
	}
}

// notifyRunObserver calls the integrity observer after COMMIT. A panic is
// swallowed for the same reason: the row is already durable.
func notifyRunObserver(observer Observer, passed bool) {
	if observer == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	observer.IntegrityRunCompleted(passed)
}
