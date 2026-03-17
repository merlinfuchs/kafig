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
	Eval        string          `json:"eval,omitempty"`
	Dispatch    *jsonDispatch   `json:"dispatch,omitempty"`
	Reset       bool            `json:"reset,omitempty"`
	RPCResponse *rpcResponseMsg `json:"rpc_response,omitempty"`
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

	forwarder := newRPCForwarder(os.Stdout)
	sess.rpcFallback = forwarder.Forward
	// Re-create instance so the fallback is wired into the router.
	if err := sess.reset(ctx); err != nil {
		printJSON(result{Error: err})
		os.Exit(1)
	}

	commands := make(chan command)
	parseErrors := make(chan error)

	// Reader goroutine: reads STDIN lines and routes them.
	go func() {
		defer close(commands)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			var input jsonInput
			if err := json.Unmarshal(scanner.Bytes(), &input); err != nil {
				parseErrors <- fmt.Errorf("invalid JSON input: %w", err)
				continue
			}

			// Route RPC responses to the forwarder.
			if input.RPCResponse != nil {
				forwarder.Deliver(*input.RPCResponse)
				continue
			}

			cmd, parseErr := parseJSONInput(input)
			if parseErr != nil {
				parseErrors <- parseErr
				continue
			}
			commands <- cmd
		}
	}()

	for {
		select {
		case parseErr := <-parseErrors:
			printJSON(result{Error: parseErr})
		case cmd, ok := <-commands:
			if !ok {
				return
			}
			printJSON(sess.exec(ctx, cmd))
		}
	}
}

func parseJSON(data []byte) (command, error) {
	var input jsonInput
	if err := json.Unmarshal(data, &input); err != nil {
		return command{}, fmt.Errorf("invalid JSON input: %w", err)
	}
	return parseJSONInput(input)
}

func parseJSONInput(input jsonInput) (command, error) {
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
