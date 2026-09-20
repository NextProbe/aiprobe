package i18n

import "fmt"

// sprint formats a non-string placeholder value. It lives in its own file so the
// core lookup logic has no formatting dependency.
func sprint(v any) string { return fmt.Sprint(v) }
