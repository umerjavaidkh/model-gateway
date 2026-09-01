package pii

import "strings"

// Restorer puts originals back into a response that arrives in pieces.
//
// # Why this needs a buffer at all
//
// A placeholder can be split across chunks. The model emits "[[PER" in one and
// "SON_1]]" in the next, and a restorer that works chunk by chunk sees neither
// as a placeholder and relays both unchanged — so the caller gets a mangled
// token where they expected a value, and nothing in the system notices.
//
// So output is held back by up to the length of the longest placeholder, and
// released once it is certain no placeholder could still be completed by what
// follows. That hold-back is the entire mechanism, and it is why this is the
// fiddliest part of the feature.
//
// # What is deliberately not attempted
//
// Nothing tries to repair a placeholder the model altered. If it paraphrases,
// translates or reformats one, restoration for that value fails and the caller
// receives the placeholder. That is visible and reportable; silently
// substituting a best guess would not be. The placeholder format is chosen to
// make alteration rare, and the round trip is asserted per provider rather
// than assumed.
type Restorer struct {
	replacements map[string]string
	held         strings.Builder
	// misses counts placeholders seen in the response that were never issued,
	// which is the signal that a model altered one.
	misses int
}

// NewRestorer returns a restorer for one request's replacements.
//
// With no replacements it is a pass-through: a redact-only request has nothing
// to restore, and buffering it would add latency for no purpose.
func NewRestorer(replacements map[string]string) *Restorer {
	return &Restorer{replacements: replacements}
}

// Active reports whether the restorer will alter anything.
func (r *Restorer) Active() bool { return len(r.replacements) > 0 }

// Push accepts a chunk and returns what is safe to emit now.
//
// The returned slice may be empty: early in a stream everything can be held
// back, and a caller must treat an empty return as "not yet" rather than as
// end of stream.
func (r *Restorer) Push(chunk []byte) []byte {
	if !r.Active() {
		return chunk
	}

	r.held.WriteString(string(chunk))
	text := r.held.String()

	// Everything up to the last point where a placeholder could still be
	// forming is safe to release. Beyond it, a fragment might be the start of
	// one that the next chunk completes.
	safe := len(text) - r.pendingTail(text)
	if safe <= 0 {
		return nil
	}

	emitted := r.substitute(text[:safe])
	r.held.Reset()
	r.held.WriteString(text[safe:])
	return []byte(emitted)
}

// Flush returns whatever is still held, at end of stream.
//
// Whatever remains cannot be completed by anything, so it is substituted and
// released as-is. A fragment that looked like the start of a placeholder was
// simply text.
func (r *Restorer) Flush() []byte {
	if !r.Active() {
		return nil
	}
	remaining := r.held.String()
	r.held.Reset()
	if remaining == "" {
		return nil
	}
	return []byte(r.substitute(remaining))
}

// Misses reports placeholders seen but never issued, which means a model
// altered one. Non-zero is worth an alert: the caller received a placeholder
// where a value was expected.
func (r *Restorer) Misses() int { return r.misses }

// pendingTail is how many trailing bytes must be held back.
//
// It looks for the last unclosed opening bracket within a placeholder's length
// of the end. Anything earlier than that cannot still be completed, because a
// placeholder is bounded — which is the property that makes a fixed-size
// hold-back sufficient rather than unbounded buffering.
func (r *Restorer) pendingTail(text string) int {
	window := MaxPlaceholderLen
	if window > len(text) {
		window = len(text)
	}
	tail := text[len(text)-window:]

	// An unclosed "[[" means a placeholder may still be forming.
	if open := strings.LastIndex(tail, placeholderOpen); open >= 0 {
		if !strings.Contains(tail[open:], placeholderClose) {
			return window - open
		}
	}
	// A single trailing "[" could become "[[" with the next chunk.
	if strings.HasSuffix(text, "[") {
		return 1
	}
	return 0
}

func (r *Restorer) substitute(text string) string {
	return placeholderPattern.ReplaceAllStringFunc(text, func(placeholder string) string {
		if original, ok := r.replacements[placeholder]; ok {
			return original
		}
		// Issued by us in shape but not in fact: the model produced something
		// placeholder-like that we never emitted. Counted and left alone.
		r.misses++
		return placeholder
	})
}
