package proc

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"

	sys "golang.org/x/sys/unix"
)

// Process statuses
const (
	StatusSleeping  = 'S'
	StatusRunning   = 'R'
	StatusTraceStop = 't'
	StatusZombie    = 'Z'

	// Kernel 2.6 has TraceStop as T
	// TODO(derekparker) Since this means something different based on the
	// version of the kernel ('T' is job control stop on modern 3.x+ kernels) we
	// may want to differentiate at some point.
	StatusTraceStopT = 'T'
)

// OSProcessDetails contains Linux specific
// process details.
type OSProcessDetails struct {
	comm string
}

// Launch creates and begins debugging a new process. First entry in
// `cmd` is the program to run, and then rest are the arguments
// to be supplied to that process.
func Launch(cmd []string) (*Process, error) {
	var (
		proc *exec.Cmd
		err  error
	)
	p := New(0)
	execOnPtraceThread(func() {
		proc = exec.Command(cmd[0])
		proc.Args = cmd
		proc.Stdout = os.Stdout
		proc.Stderr = os.Stderr
		proc.SysProcAttr = &syscall.SysProcAttr{Ptrace: true, Setpgid: true}
		err = proc.Start()
	})
	if err != nil {
		return nil, err
	}
	p.Pid = proc.Process.Pid
	_, _, err = wait4(p.Pid, p.Pid, 0, p.os.comm)
	if err != nil {
		return nil, fmt.Errorf("waiting for target execve failed: %s", err)
	}
	return initializeDebugProcess(p, proc.Path, false)
}

// Attach to an existing process with the given PID.
func Attach(pid int) (*Process, error) {
	return initializeDebugProcess(New(pid), "", true)
}

// Attach to a newly created thread, and store that thread in our list of
// known threads.
func (p *Process) addThread(tid int, attach bool) (*Thread, error) {
	if thread, ok := p.Threads[tid]; ok {
		return thread, nil
	}

	var err error
	if attach {
		execOnPtraceThread(func() { err = sys.PtraceAttach(tid) })
		if err != nil && err != sys.EPERM {
			// Do not return err if err == EPERM,
			// we may already be tracing this thread due to
			// PTRACE_O_TRACECLONE. We will surely blow up later
			// if we truly don't have permissions.
			return nil, fmt.Errorf("could not attach to new thread %d %s", tid, err)
		}
		pid, status, err := wait4(p.Pid, tid, 0, p.os.comm)
		if err != nil {
			return nil, err
		}
		if status.Exited() {
			return nil, fmt.Errorf("thread already exited %d", pid)
		}
	}

	execOnPtraceThread(func() { err = syscall.PtraceSetOptions(tid, syscall.PTRACE_O_TRACECLONE) })
	if err == syscall.ESRCH {
		if _, _, err = wait4(p.Pid, tid, 0, p.os.comm); err != nil {
			return nil, fmt.Errorf("error while waiting after adding thread: %d %s", tid, err)
		}
		execOnPtraceThread(func() { err = syscall.PtraceSetOptions(tid, syscall.PTRACE_O_TRACECLONE) })
		if err == syscall.ESRCH {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("could not set options for new traced thread %d %s", tid, err)
		}
	}

	newthread := &Thread{
		ID: tid,
		p:  p,
		os: new(OSSpecificDetails),
	}

	p.Threads[tid] = newthread
	if p.CurrentThread == nil {
		p.SetActiveThread(newthread)
	}
	return newthread, nil
}

func (p *Process) updateThreadList() error {
	tids, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*", p.Pid))
	for _, tidpath := range tids {
		tidstr := filepath.Base(tidpath)
		tid, err := strconv.Atoi(tidstr)
		if err != nil {
			return err
		}
		if _, err := p.addThread(tid, tid != p.Pid); err != nil {
			return err
		}
	}
	return nil
}

func findExecutable(pid int, path string) (string, error) {
	if path == "" {
		path = fmt.Sprintf("/proc/%d/exe", pid)
	}
	return path, nil
}

func mourn(p *Process) (int, error) {
	var status int
	for {
		_, ws, err := wait4(p.Pid, p.Pid, 0, p.os.comm)
		if err != nil {
			if err != syscall.ECHILD {
				return 0, err
			}
			break
		}
		if ws != nil && ws.Exited() {
			status = ws.ExitStatus()
			break
		}
	}
	return status, nil
}

func kill(p *Process) error {
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}

func wait(p *Process, pid int) (*WaitStatus, *Thread, error) {
	_, f, l, _ := runtime.Caller(1)
	log.WithFields(logrus.Fields{"pid": pid, "caller": fmt.Sprintf("%s:%d", filepath.Base(f), l)}).Debug("wait called")
	for {
		wpid, status, err := wait4(p.Pid, pid, 0, p.os.comm)
		if err != nil {
			return nil, nil, err
		}
		th := p.Threads[wpid]
		if status.Exited() || status.Signal() == syscall.SIGKILL {
			if wpid == p.Pid {
				_, err := Mourn(p)
				if err != nil {
					return nil, nil, err
				}
				return &WaitStatus{exited: true, exitstatus: status.ExitStatus()}, nil, nil
			}
			delete(p.Threads, wpid)
			continue
		}
		if status.StopSignal() == sys.SIGTRAP && status.TrapCause() == sys.PTRACE_EVENT_CLONE {
			// A traced thread has cloned a new thread, grab the pid and
			// add it to our list of traced threads.
			var cloned uint
			execOnPtraceThread(func() { cloned, err = sys.PtraceGetEventMsg(wpid) })
			if err != nil {
				if err == syscall.ESRCH {
					status, err := Mourn(p)
					if err != nil {
						return nil, nil, err
					}
					return nil, nil, ProcessExitedError{Pid: p.Pid, Status: status}
				}
				return nil, nil, fmt.Errorf("could not get event message: %s", err)
			}
			th, err = p.addThread(int(cloned), false)
			if err != nil {
				if err == sys.ESRCH {
					// thread died while we were adding it
					continue
				}
				return nil, nil, err
			}
			if err = th.Continue(); err != nil {
				if err == ThreadExitedErr {
					// thread died while we were adding it
					delete(p.Threads, th.ID)
					continue
				}
				return nil, nil, fmt.Errorf("could not continue new thread %d %s", cloned, err)
			}
			if err = p.Threads[int(wpid)].Continue(); err != nil {
				if err == ThreadExitedErr {
					// thread died while we were adding it
					delete(p.Threads, th.ID)
					continue
				}
				return nil, nil, fmt.Errorf("could not continue existing thread %d %s", wpid, err)
			}
			continue
		}
		if th == nil {
			// Sometimes we get an unknown thread, ignore it?
			continue
		}
		return &WaitStatus{exited: status.Exited(), exitstatus: status.ExitStatus(), signaled: true, signal: status.StopSignal()}, th, nil
	}
}

func (p *Process) loadProcessInformation() {
	comm, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/comm", p.Pid))
	if err != nil {
		fmt.Printf("Could not read process comm name: %v\n", err)
		os.Exit(1)
	}
	// removes newline character
	comm = comm[:len(comm)-1]
	p.os.comm = strings.Replace(string(comm), "%", "%%", -1)
}

func status(pid int, comm string) rune {
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return '\000'
	}
	defer f.Close()

	var (
		p     int
		state rune
	)

	// The second field of /proc/pid/stat is the name of the task in parenthesis.
	// The name of the task is the base name of the executable for this process limited to TASK_COMM_LEN characters
	// Since both parenthesis and spaces can appear inside the name of the task and no escaping happens we need to read the name of the executable first
	// See: include/linux/sched.c:315 and include/linux/sched.c:1510
	fmt.Fscanf(f, "%d ("+comm+")  %c", &p, &state)
	return state
}

func wait4(ppid, pid, options int, comm string) (int, *sys.WaitStatus, error) {
	_, f, l, _ := runtime.Caller(1)
	log.WithFields(logrus.Fields{"pid": pid, "caller": fmt.Sprintf("%s:%d", filepath.Base(f), l)}).Debug("wait4 called")
	var s sys.WaitStatus
	if (pid != ppid) || (options != 0) {
		wpid, err := sys.Wait4(pid, &s, sys.WALL|options, nil)
		return wpid, &s, err
	}
	// If we call wait4/waitpid on a thread that is the leader of its group,
	// with options == 0, while ptracing and the thread leader has exited leaving
	// zombies of its own then waitpid hangs forever this is apparently intended
	// behaviour in the linux kernel because it's just so convenient.
	// Therefore we call wait4 in a loop with WNOHANG, sleeping a while between
	// calls and exiting when either wait4 succeeds or we find out that the thread
	// has become a zombie.
	// References:
	// https://sourceware.org/bugzilla/show_bug.cgi?id=12702
	// https://sourceware.org/bugzilla/show_bug.cgi?id=10095
	// https://sourceware.org/bugzilla/attachment.cgi?id=5685
	for {
		wpid, err := sys.Wait4(pid, &s, sys.WNOHANG|sys.WALL|options, nil)
		if err != nil {
			return 0, nil, err
		}
		if wpid != 0 {
			return wpid, &s, err
		}
		if status(pid, comm) == StatusZombie {
			// TODO(derekparker) properly handle when group leader becomes a zombie.
			log.WithField("pid", pid).Debug("process is a zombie")
			return pid, &s, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}
