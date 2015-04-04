package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	sys "golang.org/x/sys/unix"

	"github.com/derekparker/delve/command"
	"github.com/derekparker/delve/proctl"

	"github.com/peterh/liner"
)

const configDir = ".dlv"
const historyFile string = ".dbg_history"

func Run(args []string) {
	var (
		dbp *proctl.DebuggedProcess
		err error
		t   = &Term{prompt: "(dlv) ", line: liner.NewLiner()}
	)
	defer t.line.Close()

	// ensure the config directory is created
	err = createConfigPath()
	if err != nil {
		t.die(1, "Could not create config directory: ", err)
	}

	switch args[0] {
	case "run":
		const debugname = "debug"
		cmd := exec.Command("go", "build", "-o", debugname, "-gcflags", "-N -l")
		err := cmd.Run()
		if err != nil {
			t.die(1, "Could not compile program:", err)
		}
		defer os.Remove(debugname)

		dbp, err = proctl.Launch(append([]string{"./" + debugname}, args...))
		if err != nil {
			t.die(1, "Could not launch program:", err)
		}
	case "test":
		wd, err := os.Getwd()
		if err != nil {
			t.die(1, err)
		}
		base := filepath.Base(wd)
		cmd := exec.Command("go", "test", "-c", "-gcflags", "-N -l")
		err = cmd.Run()
		if err != nil {
			t.die(1, "Could not compile program:", err)
		}
		debugname := "./" + base + ".test"
		defer os.Remove(debugname)

		dbp, err = proctl.Launch(append([]string{debugname}, args...))
		if err != nil {
			t.die(1, "Could not launch program:", err)
		}
	case "attach":
		pid, err := strconv.Atoi(args[1])
		if err != nil {
			t.die(1, "Invalid pid", args[1])
		}
		dbp, err = proctl.Attach(pid)
		if err != nil {
			t.die(1, "Could not attach to process:", err)
		}
	default:
		dbp, err = proctl.Launch(args)
		if err != nil {
			t.die(1, "Could not launch program:", err)
		}
	}

	ch := make(chan os.Signal)
	signal.Notify(ch, sys.SIGINT)
	go func() {
		for _ = range ch {
			if dbp.Running() {
				dbp.RequestManualStop()
			}
		}
	}()

	cmds := command.DebugCommands()
	fullHistoryFile, err := getConfigFilePath(historyFile)
	if err != nil {
		t.die(1, "Error loading history file:", err)
	}

	f, err := os.Open(fullHistoryFile)
	if err != nil {
		f, _ = os.Create(fullHistoryFile)
	}
	t.line.ReadHistory(f)
	f.Close()
	fmt.Println("Type 'help' for list of commands.")

	for {
		cmdstr, err := t.promptForInput()
		if err != nil {
			if err == io.EOF {
				handleExit(dbp, t, 0)
			}
			t.die(1, "Prompt for input failed.\n")
		}

		exited, err := cmds.RunCommand(dbp, cmdstr)
		if exited {
			handleExit(dbp, t, 0)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Command failed: %s\n", err)
		}
	}
}

func handleExit(dbp *proctl.DebuggedProcess, t *Term, status int) {
	fullHistoryFile, err := getConfigFilePath(historyFile)
	if err != nil {
		fmt.Println("Error saving history file:", err)
	} else {
		if f, err := os.OpenFile(fullHistoryFile, os.O_RDWR, 0666); err == nil {
			_, err := t.line.WriteHistory(f)
			if err != nil {
				fmt.Println("readline history error: ", err)
			}
			f.Close()
		}
	}

	if !dbp.Exited() {
		for _, bp := range dbp.HWBreakPoints {
			if bp == nil {
				continue
			}
			if _, err := dbp.Clear(bp.Addr); err != nil {
				fmt.Printf("Can't clear breakpoint @%x: %s\n", bp.Addr, err)
			}
		}

		for pc := range dbp.BreakPoints {
			if _, err := dbp.Clear(pc); err != nil {
				fmt.Printf("Can't clear breakpoint @%x: %s\n", pc, err)
			}
		}

		answer, err := t.line.Prompt("Would you like to kill the process? [y/n]")
		if err != nil {
			t.die(2, io.EOF)
		}
		answer = strings.TrimSuffix(answer, "\n")

		fmt.Println("Detaching from process...")
		err = sys.PtraceDetach(dbp.Process.Pid)
		if err != nil {
			t.die(2, "Could not detach", err)
		}

		if answer == "y" {
			fmt.Println("Killing process", dbp.Process.Pid)

			err := dbp.Process.Kill()
			if err != nil {
				fmt.Println("Could not kill process", err)
			}
		}
	}

	t.die(status, "Hope I was of service hunting your bug!")
}

type Term struct {
	prompt string
	line   *liner.State
}

func (t *Term) die(status int, args ...interface{}) {
	if t.line != nil {
		t.line.Close()
	}

	fmt.Fprint(os.Stderr, args)
	fmt.Fprint(os.Stderr, "\n")
	os.Exit(status)
}

func (t *Term) promptForInput() (string, error) {
	l, err := t.line.Prompt(t.prompt)
	if err != nil {
		return "", err
	}

	l = strings.TrimSuffix(l, "\n")
	if l != "" {
		t.line.AppendHistory(l)
	}

	return l, nil
}

func createConfigPath() error {
	path, err := getConfigFilePath("")
	if err != nil {
		return err
	}
	return os.MkdirAll(path, 0700)
}

func getConfigFilePath(file string) (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	return path.Join(usr.HomeDir, configDir, file), nil
}
