package kafig

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ErrorCode classifies runtime-level execution errors.
type ErrorCode string

const (
	ErrorCodeCPULimitExceeded    ErrorCode = "cpu_limit_exceeded"
	ErrorCodeMemoryLimitExceeded ErrorCode = "memory_limit_exceeded"
	ErrorCodeStackOverflow       ErrorCode = "stack_overflow"
	ErrorCodeRuntimeError        ErrorCode = "runtime_error"
)

// JsError is returned when JavaScript code throws an exception.
type JsError struct {
	Name    string  `json:"name"`
	Message string  `json:"message"`
	Stack   *string `json:"stack"`
}

func (e *JsError) Error() string {
	return fmt.Sprintf("%s: %s", e.Name, e.Message)
}

// RuntimeError is returned for resource limit violations and internal errors.
type RuntimeError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *RuntimeError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// parseErrorJSON unmarshals a JSON error payload produced by the WASM runtime
// into either a *JsError or *RuntimeError based on the "kind" field.
func parseErrorJSON(data []byte) error {
	var peek struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return &RuntimeError{
			Code:    ErrorCodeRuntimeError,
			Message: string(data),
		}
	}

	switch peek.Kind {
	case "js_error":
		var jsErr JsError
		if err := json.Unmarshal(data, &jsErr); err != nil {
			return &RuntimeError{
				Code:    ErrorCodeRuntimeError,
				Message: string(data),
			}
		}
		return &jsErr
	case "runtime_error":
		var rtErr RuntimeError
		if err := json.Unmarshal(data, &rtErr); err != nil {
			return &RuntimeError{
				Code:    ErrorCodeRuntimeError,
				Message: string(data),
			}
		}
		// Re-classify on the Go side as a safety net.
		if code := classifyErrorCode(rtErr.Message); rtErr.Code == "" || (rtErr.Code == ErrorCodeRuntimeError && code != ErrorCodeRuntimeError) {
			rtErr.Code = code
		}
		return &rtErr
	default:
		return &RuntimeError{
			Code:    ErrorCodeRuntimeError,
			Message: string(data),
		}
	}
}

// classifyErrorCode maps a QuickJS error message to an ErrorCode. This acts
// as a safety net for cases where the Rust classifier can't run (e.g. when
// the interrupt flag aborts the catch block).
func classifyErrorCode(msg string) ErrorCode {
	if msg == "interrupted" {
		return ErrorCodeCPULimitExceeded
	}
	if strings.Contains(msg, "out of memory") {
		return ErrorCodeMemoryLimitExceeded
	}
	if strings.Contains(msg, "stack") {
		return ErrorCodeStackOverflow
	}
	return ErrorCodeRuntimeError
}
