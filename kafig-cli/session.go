package main

import (
	"context"
	"encoding/json"
	"fmt"

	kafig "github.com/merlinfuchs/kafig/kafig-go"
)

// command represents a parsed user action, produced by both interactive and JSON input parsing.
type command struct {
	Type   string          // "eval", "dispatch", "reset", "clear", "exit", "help"
	Source string          // eval: JS source
	Name   string          // dispatch: event name
	Params json.RawMessage // dispatch: event params
}

// result represents the outcome of executing a command.
type result struct {
	Value json.RawMessage
	Error error
	Stats *kafig.ExecutionStats
	Text  string
}

// session holds the kafig runtime and instance, providing a single exec method for all commands.
type session struct {
	rt   *kafig.Runtime
	inst *kafig.Instance
}

func newSession(ctx context.Context) (*session, error) {
	rt, err := kafig.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create runtime: %w", err)
	}

	s := &session{rt: rt}
	if err := s.reset(ctx); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("failed to create instance: %w", err)
	}
	return s, nil
}

func (s *session) close(ctx context.Context) {
	if s.inst != nil {
		s.inst.Close(ctx)
	}
	if s.rt != nil {
		s.rt.Close(ctx)
	}
}

func (s *session) reset(ctx context.Context) error {
	if s.inst != nil {
		s.inst.Close(context.Background())
	}
	router := kafig.NewRPCRouter()
	var err error
	s.inst, err = s.rt.Instance(ctx,
		kafig.WithRouter(router),
		kafig.WithInterruptCallback(func(opcodes, cpuTimeUs uint64) bool {
			return (maxOpcodes > 0 && opcodes > maxOpcodes) || (maxCPUTimeUs > 0 && cpuTimeUs > maxCPUTimeUs)
		}),
	)
	return err
}

func (s *session) exec(ctx context.Context, cmd command) result {
	switch cmd.Type {
	case "eval":
		return s.execEval(ctx, cmd.Source)
	case "dispatch":
		return s.execDispatch(ctx, cmd.Name, cmd.Params)
	case "reset":
		if err := s.reset(ctx); err != nil {
			return result{Error: err}
		}
		return result{Text: "Instance reset — all JS state cleared."}
	case "help":
		return result{Text: helpText}
	case "clear":
		return result{Text: "\033[H\033[2J"}
	case "exit":
		return result{Text: "Goodbye!"}
	default:
		return result{Error: fmt.Errorf("unknown command: %s", cmd.Type)}
	}
}

func (s *session) execEval(ctx context.Context, source string) result {
	if err := s.inst.ResetExecutionStats(ctx); err != nil {
		return result{Error: fmt.Errorf("failed to reset stats: %w", err)}
	}

	value, evalErr := s.inst.Eval(ctx, source, kafig.WithAsync())

	var r result
	if stats, err := s.inst.GetExecutionStats(ctx); err == nil {
		r.Stats = &stats
	}
	if evalErr != nil {
		r.Error = evalErr
	} else {
		r.Value = value
	}
	return r
}

func (s *session) execDispatch(ctx context.Context, name string, params json.RawMessage) result {
	if err := s.inst.ResetExecutionStats(ctx); err != nil {
		return result{Error: fmt.Errorf("failed to reset stats: %w", err)}
	}

	value, evalErr := s.inst.DispatchEvent(ctx, name, params, kafig.WithAsync())

	var r result
	if stats, err := s.inst.GetExecutionStats(ctx); err == nil {
		r.Stats = &stats
	}
	if evalErr != nil {
		r.Error = evalErr
	} else {
		r.Value = value
	}
	return r
}

const helpText = `Commands:
  .help                        Show this help message
  .dispatch <name> [params]    Dispatch an event to a registered handler
  .reset                       Reset the JS instance (clears all state)
  .clear                       Clear the screen
  .exit                        Exit the REPL

Write JavaScript and press Enter to evaluate.
Use host.result(value) to return a value.
Use host.on(name, fn) to register event handlers.
Variables and functions persist across evals.`
