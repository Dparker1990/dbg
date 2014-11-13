// Package command implements functions for responding to user
// input and dispatching to appropriate backend commands.
package command

import (
	"bufio"
	"debug/gosym"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/derekparker/delve/proctl"
)

type cmdfunc func(proc *proctl.DebuggedProcess, args ...string) error

type Commands struct {
	cmds map[string]cmdfunc
}

// Returns a Commands struct with default commands defined.
func DebugCommands() *Commands {
	cmds := map[string]cmdfunc{
		"help":       help,
		"continue":   cont,
		"next":       next,
		"break":      breakpoint,
		"step":       step,
		"clear":      clear,
		"print":      printVar,
		"threads":    threads,
		"goroutines": goroutines,
		"":           nullCommand,
	}

	return &Commands{cmds}
}

// Register custom commands. Expects cf to be a func of type cmdfunc,
// returning only an error.
func (c *Commands) Register(cmdstr string, cf cmdfunc) {
	c.cmds[cmdstr] = cf
}

// Find will look up the command function for the given command input.
// If it cannot find the command it will defualt to noCmdAvailable().
// If the command is an empty string it will replay the last command.
func (c *Commands) Find(cmdstr string) cmdfunc {
	cmd, ok := c.cmds[cmdstr]
	if !ok {
		return noCmdAvailable
	}

	// Allow <enter> to replay last command
	c.cmds[""] = cmd

	return cmd
}

func CommandFunc(fn func() error) cmdfunc {
	return func(p *proctl.DebuggedProcess, args ...string) error {
		return fn()
	}
}

func noCmdAvailable(p *proctl.DebuggedProcess, ars ...string) error {
	return fmt.Errorf("command not available")
}

func nullCommand(p *proctl.DebuggedProcess, ars ...string) error {
	return nil
}

func help(p *proctl.DebuggedProcess, ars ...string) error {
	fmt.Println(`The following commands are available:
    break - Set break point at the entry point of a function, or at a specific file/line. Example: break foo.go:13.
    continue - Run until breakpoint or program termination.
    step - Single step through program.
    next - Step over to next source line.
    threads - Print out info for every traced thread.
    goroutines - Print out info for every goroutine.
    print $var - Evaluate a variable.
    exit - Exit the debugger.`)

	return nil
}

func threads(p *proctl.DebuggedProcess, ars ...string) error {
	return p.PrintThreadInfo()
}

func goroutines(p *proctl.DebuggedProcess, ars ...string) error {
	return p.PrintGoroutinesInfo()
}

func cont(p *proctl.DebuggedProcess, ars ...string) error {
	err := p.Continue()
	if err != nil {
		return err
	}

	return printcontext(p)
}

func step(p *proctl.DebuggedProcess, args ...string) error {
	err := p.Step()
	if err != nil {
		return err
	}

	return printcontext(p)
}

func next(p *proctl.DebuggedProcess, args ...string) error {
	err := p.Next()
	if err != nil {
		return err
	}

	return printcontext(p)
}

func clear(p *proctl.DebuggedProcess, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("not enough arguments")
	}

	var (
		fn    *gosym.Func
		pc    uint64
		fname = args[0]
	)

	if strings.ContainsRune(fname, ':') {
		fl := strings.Split(fname, ":")

		f, err := filepath.Abs(fl[0])
		if err != nil {
			return err
		}

		l, err := strconv.Atoi(fl[1])
		if err != nil {
			return err
		}

		pc, fn, err = p.GoSymTable.LineToPC(f, l)
		if err != nil {
			return err
		}
	} else {
		fn = p.GoSymTable.LookupFunc(fname)
		if fn == nil {
			return fmt.Errorf("No function named %s", fname)
		}

		pc = fn.Entry
	}

	bp, err := p.Clear(pc)
	if err != nil {
		return err
	}

	fmt.Printf("Breakpoint cleared at %#v for %s %s:%d\n", bp.Addr, bp.FunctionName, bp.File, bp.Line)

	return nil
}

func breakpoint(p *proctl.DebuggedProcess, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("not enough arguments")
	}

	var (
		fn    *gosym.Func
		pc    uint64
		fname = args[0]
	)

	if strings.ContainsRune(fname, ':') {
		fl := strings.Split(fname, ":")

		f, err := filepath.Abs(fl[0])
		if err != nil {
			return err
		}

		l, err := strconv.Atoi(fl[1])
		if err != nil {
			return err
		}

		pc, fn, err = p.GoSymTable.LineToPC(f, l)
		if err != nil {
			return err
		}
	} else {
		fn = p.GoSymTable.LookupFunc(fname)
		if fn == nil {
			return fmt.Errorf("No function named %s", fname)
		}

		pc = fn.Entry
	}

	bp, err := p.Break(uintptr(pc))
	if err != nil {
		return err
	}

	fmt.Printf("Breakpoint set at %#v for %s %s:%d\n", bp.Addr, bp.FunctionName, bp.File, bp.Line)

	return nil
}

func printVar(p *proctl.DebuggedProcess, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("not enough arguments")
	}

	val, err := p.EvalSymbol(args[0])
	if err != nil {
		return err
	}

	fmt.Println(val.Value)
	return nil
}

func printcontext(p *proctl.DebuggedProcess) error {
	var context []string

	regs, err := p.Registers()
	if err != nil {
		return err
	}

	f, l, fn := p.GoSymTable.PCToLine(regs.PC())

	if fn != nil {
		fmt.Printf("Stopped at: %s:%d\n", f, l)
		file, err := os.Open(f)
		if err != nil {
			return err
		}
		defer file.Close()

		buf := bufio.NewReader(file)
		for i := 1; i < l-5; i++ {
			_, err := buf.ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
		}

		for i := l - 5; i <= l+5; i++ {
			line, err := buf.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					return err
				}

				if err == io.EOF {
					break
				}
			}

			if i == l {
				line = "\033[34m=>\033[0m" + line
			}

			context = append(context, fmt.Sprintf("\033[34m%d\033[0m: %s", i, line))
		}
	} else {
		fmt.Printf("Stopped at: 0x%x\n", regs.PC())
		context = append(context, "\033[34m=>\033[0m    no source available")
	}

	fmt.Println(strings.Join(context, ""))

	return nil
}
