package main

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// validateCommandSyntax parses command as a shell script and returns a non-nil
// error if it is syntactically invalid (unbalanced quotes, a dangling pipe, an
// unterminated `$(`, etc.). It never executes anything — the parser only reads
// the grammar — so it is safe to run on untrusted input in the controller.
//
// The executor runs the command as `sh -c <command>`, so a syntax error here
// guarantees the command can never run. Catching it at submission/acceptance
// time short-circuits the human-approval round-trip for a request that was
// doomed anyway.
//
// We parse in the bash language variant deliberately: it accepts a superset of
// POSIX sh, so we only reject input that is broken in *every* shell. The
// executor's busybox `ash` is stricter than bash for a handful of extensions,
// so a command can still fail at runtime; this check is a cheap early filter
// for obvious typos, not a guarantee of executability. The human reviewer
// remains the trust boundary.
func validateCommandSyntax(command string) error {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	if _, err := parser.Parse(strings.NewReader(command), ""); err != nil {
		return fmt.Errorf("invalid shell syntax: %w", err)
	}
	return nil
}
