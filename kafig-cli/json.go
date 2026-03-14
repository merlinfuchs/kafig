package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	kafig "github.com/merlinfuchs/kafig/kafig-go"
)

type jsonInput struct {
	Eval     string        `json:"eval,omitempty"`
	Dispatch *jsonDispatch `json:"dispatch,omitempty"`
	Reset    bool          `json:"reset,omitempty"`
}

type jsonDispatch struct {
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params,omitempty"`
}

type jsonOutput struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *jsonError      `json:"error,omitempty"`
	Stats  *jsonStats      `json:"stats,omitempty"`
	Text   string          `json:"text,omitempty"`
}

type jsonError struct {
	Message   string `json:"message"`
	ErrorType string `json:"errorType,omitempty"`
	Stack     string `json:"stack,omitempty"`
}

type jsonStats struct {
	Opcodes   uint64 `json:"opcodes"`
	CPUTimeUs uint64 `json:"cpuTimeUs"`
}

func runJSON(ctx context.Context) {
	var err error
	sess, err = newSession(ctx)
	if err != nil {
		printJSON(result{Error: err})
		os.Exit(1)
	}
	defer sess.close(context.Background())

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		cmd, parseErr := parseJSON(scanner.Bytes())
		if parseErr != nil {
			printJSON(result{Error: parseErr})
			continue
		}
		printJSON(sess.exec(ctx, cmd))
	}
}

func parseJSON(data []byte) (command, error) {
	var input jsonInput
	if err := json.Unmarshal(data, &input); err != nil {
		return command{}, fmt.Errorf("invalid JSON input: %w", err)
	}

	switch {
	case input.Eval != "":
		return command{Type: "eval", Source: input.Eval}, nil
	case input.Dispatch != nil:
		params := input.Dispatch.Params
		if params == nil {
			params = json.RawMessage("null")
		}
		return command{Type: "dispatch", Name: input.Dispatch.Name, Params: params}, nil
	case input.Reset:
		return command{Type: "reset"}, nil
	default:
		return command{}, fmt.Errorf("input must have \"eval\", \"dispatch\", or \"reset\" field")
	}
}

func printJSON(r result) {
	var out jsonOutput

	if r.Text != "" {
		out.Text = r.Text
	}

	if r.Error != nil {
		var scriptErr *kafig.ScriptError
		if errors.As(r.Error, &scriptErr) {
			out.Error = &jsonError{
				Message:   scriptErr.Message,
				ErrorType: string(scriptErr.ErrorType),
			}
			if scriptErr.Stack != nil {
				out.Error.Stack = *scriptErr.Stack
			}
		} else {
			out.Error = &jsonError{Message: r.Error.Error()}
		}
	} else if r.Value != nil {
		out.Result = r.Value
	}

	if r.Stats != nil {
		out.Stats = &jsonStats{Opcodes: r.Stats.Opcodes, CPUTimeUs: r.Stats.CPUTimeUs}
	}

	data, _ := json.Marshal(out)
	fmt.Println(string(data))
}
