package config

import (
	"fmt"
	"strconv"
	"time"
)

// knob is one env-tunable Host setting, declared as a row of Host.knobs.
type knob struct {
	key string
	// resolve writes the knob's value into its field from raw, the env value
	// ("" when unset).
	resolve func(raw string) error
}

// strictness is what a knob does with a value its parser rejects.
type strictness bool

const (
	// lenient falls back to the default, as if the var were unset.
	lenient strictness = false
	// strict fails Load with an error naming the var and what it accepts.
	strict strictness = true
)

// parser turns a set env value into a T, reporting false for a value it rejects.
// want says what it accepts, for a strict knob's error.
type parser[T any] struct {
	parse func(raw string) (T, bool)
	want  string
}

var (
	positiveMillis = parser[time.Duration]{millis(1), "a positive integer (milliseconds)"}
	ceilingMillis  = parser[time.Duration]{millis(0), "a non-negative integer (milliseconds; 0 = unlimited)"}
	positiveCount  = parser[int]{count(1), "a positive integer"}
	ceilingCount   = parser[int]{count(0), "a non-negative integer (0 = unlimited)"}
	// reclaimBytes accepts 0 as "disable reclaim", a documented value. ParseUint
	// rejects a leading '-', so a negative fails here too.
	reclaimBytes = parser[uint64]{
		func(raw string) (uint64, bool) {
			n, err := strconv.ParseUint(raw, 10, 64)
			return n, err == nil
		},
		"a non-negative integer (bytes; 0 = disable reclaim)",
	}
	// text takes any set value as-is.
	text = parser[string]{func(raw string) (string, bool) { return raw, true }, ""}
)

// bind declares a knob: env var key sets *dst via p, def applies when key is
// unset, and s decides what a rejected value does.
func bind[T any](key string, dst *T, p parser[T], def T, s strictness) knob {
	return knob{key: key, resolve: func(raw string) error {
		if raw == "" {
			*dst = def
			return nil
		}
		v, ok := p.parse(raw)
		switch {
		case ok:
			*dst = v
		case s == lenient:
			*dst = def
		default:
			return fmt.Errorf("%s must be %s, got %q", key, p.want, raw)
		}
		return nil
	}}
}

// count parses an integer of at least min.
func count(min int) func(string) (int, bool) {
	return func(raw string) (int, bool) {
		n, err := strconv.Atoi(raw)
		return n, err == nil && n >= min
	}
}

// millis parses a millisecond count of at least min into a Duration.
func millis(min int) func(string) (time.Duration, bool) {
	parse := count(min)
	return func(raw string) (time.Duration, bool) {
		n, ok := parse(raw)
		return time.Duration(n) * time.Millisecond, ok
	}
}
