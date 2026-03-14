package kafig

import (
	"fmt"
	"strings"
)

// ErrorType classifies JavaScript execution errors.
type ErrorType string

const (
	ErrorTypeCPULimitExceeded    ErrorType = "cpu_limit_exceeded"
	ErrorTypeMemoryLimitExceeded ErrorType = "memory_limit_exceeded"
	ErrorTypeStackOverflow       ErrorType = "stack_overflow"
	ErrorTypeRuntimeError        ErrorType = "runtime_error"
)

// ScriptError is returned when JavaScript execution fails. It carries the
// error message, classification, and optional stack trace from QuickJS.
type ScriptError struct {
	Message   string    `json:"error"`
	ErrorType ErrorType `json:"errorType"`
	Stack     *string   `json:"stack"`
}

func (e *ScriptError) Error() string {
	return fmt.Sprintf("%s: %s", e.ErrorType, e.Message)
}

// classifyError maps a QuickJS error message to an ErrorType. This mirrors
// the JS-side __classifyError and acts as a safety net for cases where the
// JS classifier can't run (e.g. when the interrupt flag aborts the catch block).
func classifyError(msg string) ErrorType {
	if msg == "interrupted" {
		return ErrorTypeCPULimitExceeded
	}
	if strings.Contains(msg, "out of memory") {
		return ErrorTypeMemoryLimitExceeded
	}
	if strings.Contains(msg, "stack") {
		return ErrorTypeStackOverflow
	}
	return ErrorTypeRuntimeError
}
