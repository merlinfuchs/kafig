package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	prompt "github.com/elk-language/go-prompt"
	pstrings "github.com/elk-language/go-prompt/strings"

	kafig "github.com/merlinfuchs/kafig/kafig-go"
)

var (
	styleResult = lipgloss.NewStyle().Foreground(lipgloss.Color("#98c379"))
	styleError  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e06c75")).Bold(true)
	styleStack  = lipgloss.NewStyle().Foreground(lipgloss.Color("#5c6370"))
	styleStats  = lipgloss.NewStyle().Foreground(lipgloss.Color("#5c6370"))
	styleBanner = lipgloss.NewStyle().Foreground(lipgloss.Color("#61afef")).Bold(true)
	styleEmpty  = lipgloss.NewStyle().Foreground(lipgloss.Color("#5c6370")).Italic(true)
)

var (
	multiLine   strings.Builder
	inMultiLine bool
)

func runInteractive(ctx context.Context) {
	var err error
	sess, err = newSession(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer sess.close(context.Background())

	fmt.Println(styleBanner.Render("kafig") + " interactive JavaScript runtime")
	fmt.Println(styleStats.Render("Type .help for commands, Ctrl+D to exit"))
	fmt.Println()

	p := prompt.New(
		executor,
		prompt.WithPrefix("kafig> "),
		prompt.WithCompleter(completer),
		prompt.WithExecuteOnEnterCallback(shouldExecute),
	)
	p.Run()
}

// parseInteractive converts raw user input (dot-commands or JS) into a command.
// Returns nil when the input requires special error handling in the executor.
func parseInteractive(input string) *command {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	if !strings.HasPrefix(input, ".") {
		return &command{Type: "eval", Source: input}
	}

	parts := strings.SplitN(input, " ", 3)
	switch parts[0] {
	case ".help":
		return &command{Type: "help"}
	case ".dispatch":
		if len(parts) < 2 {
			return nil
		}
		params := json.RawMessage("null")
		if len(parts) == 3 {
			raw := strings.TrimSpace(parts[2])
			if !json.Valid([]byte(raw)) {
				return nil
			}
			params = json.RawMessage(raw)
		}
		return &command{Type: "dispatch", Name: parts[1], Params: params}
	case ".reset":
		return &command{Type: "reset"}
	case ".clear":
		return &command{Type: "clear"}
	case ".exit":
		return &command{Type: "exit"}
	default:
		return nil
	}
}

func printInteractive(r result) {
	if r.Text != "" {
		if r.Text == "\033[H\033[2J" {
			fmt.Print(r.Text)
		} else {
			fmt.Println(r.Text)
		}
		if r.Text != helpText {
			fmt.Println()
		}
		return
	}

	if r.Error != nil {
		var scriptErr *kafig.ScriptError
		if errors.As(r.Error, &scriptErr) {
			fmt.Println(styleError.Render(fmt.Sprintf("%s: %s", scriptErr.ErrorType, scriptErr.Message)))
			if scriptErr.Stack != nil && *scriptErr.Stack != "" {
				fmt.Println(styleStack.Render(*scriptErr.Stack))
			}
		} else {
			fmt.Println(styleError.Render(r.Error.Error()))
		}
	} else if r.Value != nil {
		var pretty json.RawMessage
		if err := json.Unmarshal(r.Value, &pretty); err == nil {
			if indented, err := json.MarshalIndent(pretty, "   ", "  "); err == nil {
				fmt.Println(styleResult.Render("=> " + string(indented)))
			} else {
				fmt.Println(styleResult.Render("=> " + string(r.Value)))
			}
		} else {
			fmt.Println(styleResult.Render("=> " + string(r.Value)))
		}
	} else {
		fmt.Println(styleEmpty.Render("   (no result)"))
	}

	if r.Stats != nil {
		fmt.Println(styleStats.Render("   " + formatStats(r.Stats.Opcodes, r.Stats.CPUTimeUs)))
	}
	fmt.Println()
}

func executor(input string) {
	ctx := context.Background()

	if inMultiLine {
		multiLine.WriteString("\n")
		multiLine.WriteString(input)
		if isComplete(multiLine.String()) {
			source := multiLine.String()
			multiLine.Reset()
			inMultiLine = false
			printInteractive(sess.exec(ctx, command{Type: "eval", Source: source}))
		}
		return
	}

	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return
	}

	// Dot commands
	if strings.HasPrefix(trimmed, ".") {
		parts := strings.SplitN(trimmed, " ", 3)

		if parts[0] == ".dispatch" && len(parts) < 2 {
			fmt.Println(styleError.Render("usage: .dispatch <name> [json_params]"))
			fmt.Println(styleStats.Render("  example: .dispatch greet {\"name\": \"world\"}"))
			return
		}
		if parts[0] == ".dispatch" && len(parts) == 3 {
			raw := strings.TrimSpace(parts[2])
			if !json.Valid([]byte(raw)) {
				fmt.Println(styleError.Render("invalid JSON params: " + raw))
				return
			}
		}

		cmd := parseInteractive(trimmed)
		if cmd == nil {
			fmt.Println(styleError.Render(fmt.Sprintf("unknown command: %s", parts[0])))
			return
		}

		r := sess.exec(ctx, *cmd)
		if cmd.Type == "exit" {
			fmt.Println(r.Text)
			os.Exit(0)
		}
		printInteractive(r)
		return
	}

	// Multi-line: incomplete input
	if !isComplete(trimmed) {
		inMultiLine = true
		multiLine.Reset()
		multiLine.WriteString(trimmed)
		return
	}

	printInteractive(sess.exec(ctx, command{Type: "eval", Source: trimmed}))
}

func completer(d prompt.Document) ([]prompt.Suggest, pstrings.RuneNumber, pstrings.RuneNumber) {
	endIndex := d.CurrentRuneIndex()
	w := d.GetWordBeforeCursor()
	startIndex := endIndex - pstrings.RuneCount([]byte(w))

	if !strings.HasPrefix(d.TextBeforeCursor(), ".") {
		return nil, 0, 0
	}

	suggestions := []prompt.Suggest{
		{Text: ".help", Description: "Show available commands"},
		{Text: ".dispatch", Description: "Dispatch event: .dispatch <name> [json_params]"},
		{Text: ".reset", Description: "Reset JS instance (clear all state)"},
		{Text: ".clear", Description: "Clear the screen"},
		{Text: ".exit", Description: "Exit the REPL"},
	}

	return prompt.FilterHasPrefix(suggestions, w, true), startIndex, endIndex
}

// shouldExecute determines if Enter should execute or insert a newline.
func shouldExecute(p *prompt.Prompt, indentSize int) (int, bool) {
	input := p.Buffer().Text()

	if inMultiLine {
		if isComplete(multiLine.String() + "\n" + input) {
			return 0, true
		}
		return indentSize, false
	}

	trimmed := strings.TrimSpace(input)

	if trimmed == "" || strings.HasPrefix(trimmed, ".") {
		return 0, true
	}

	if isComplete(trimmed) {
		return 0, true
	}

	return indentSize, false
}

// isComplete checks if brackets/parens/braces are balanced.
func isComplete(input string) bool {
	depth := 0
	inString := false
	stringChar := byte(0)
	escaped := false

	for i := 0; i < len(input); i++ {
		c := input[i]

		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && inString {
			escaped = true
			continue
		}

		if inString {
			if c == stringChar {
				inString = false
			}
			continue
		}

		switch c {
		case '"', '\'', '`':
			inString = true
			stringChar = c
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			depth--
		}
	}

	return depth <= 0 && !inString
}

func formatStats(opcodes, cpuTimeUs uint64) string {
	cpuMs := float64(cpuTimeUs) / 1000.0
	return fmt.Sprintf("%s opcodes | %.2fms CPU", formatNumber(opcodes), cpuMs)
}

func formatNumber(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
