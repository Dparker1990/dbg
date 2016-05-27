package proc

// #include "proc_darwin.h"
// #include "threads_darwin.h"
// #include "exec_darwin.h"
// #include <stdlib.h>
import "C"
import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/Sirupsen/logrus"

	sys "golang.org/x/sys/unix"
)

// OSProcessDetails holds Darwin specific information.
type OSProcessDetails struct {
	task             C.task_t      // mach task for the debugged process.
	exceptionPort    C.mach_port_t // mach port for receiving mach exceptions.
	notificationPort C.mach_port_t // mach port for dead name notification (process exit).

	// the main port we use, will return messages from both the
	// exception and notification ports.
	portSet C.mach_port_t
}

// Launch creates and begins debugging a new process. Uses a
// custom fork/exec process in order to take advantage of
// PT_SIGEXC on Darwin which will turn Unix signals into
// Mach exceptions.
func Launch(cmd []string) (*Process, error) {
	argv0Go, err := filepath.Abs(cmd[0])
	if err != nil {
		return nil, err
	}
	// Make sure the binary exists.
	if filepath.Base(cmd[0]) == cmd[0] {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(argv0Go); err != nil {
		return nil, err
	}

	argv0 := C.CString(argv0Go)
	argvSlice := make([]*C.char, 0, len(cmd)+1)
	for _, arg := range cmd {
		argvSlice = append(argvSlice, C.CString(arg))
	}
	// argv array must be null terminated.
	argvSlice = append(argvSlice, nil)

	p := New(0)
	var pid int
	execOnPtraceThread(func() {
		ret := C.fork_exec(argv0, &argvSlice[0], C.int(len(argvSlice)),
			&p.os.task, &p.os.portSet, &p.os.exceptionPort,
			&p.os.notificationPort)
		pid = int(ret)
	})
	if pid <= 0 {
		return nil, fmt.Errorf("could not fork/exec")
	}
	p.Pid = pid
	for i := range argvSlice {
		C.free(unsafe.Pointer(argvSlice[i]))
	}

	var hdr C.mach_msg_header_t
	var sig C.int
	port := C.mach_port_wait(p.os.portSet, &hdr, &sig, C.int(0))
	if port == 0 {
		return nil, errors.New("error while waiting for process to exec")
	}
	p, err = initializeDebugProcess(p, argv0Go, false)
	if err != nil {
		return p, err
	}
	th, ok := p.Threads[int(port)]
	if !ok {
		return nil, errors.New("could not find thread")
	}
	th.os.msgStop = true
	th.os.hdr = hdr
	th.os.sig = sig

	return p, nil
}

// Attach to an existing process with the given PID.
func Attach(pid int) (*Process, error) {
	p := New(pid)

	kret := C.acquire_mach_task(C.int(pid),
		&p.os.task, &p.os.portSet, &p.os.exceptionPort,
		&p.os.notificationPort)

	if kret != C.KERN_SUCCESS {
		return nil, fmt.Errorf("could not attach to %d", pid)
	}

	return initializeDebugProcess(p, "", true)
}

// Kill kills the process.
func kill(p *Process) error {
	if p.exited {
		return nil
	}
	if err := Stop(p); err != nil {
		return err
	}
	for _, th := range p.Threads {
		th.sendMachReply()
	}
	kret := C.task_set_exception_ports(p.os.task, C.EXC_MASK_ALL, C.MACH_PORT_NULL, C.EXCEPTION_DEFAULT, C.THREAD_STATE_NONE)
	if kret != C.KERN_SUCCESS {
		return errors.New("could not restore exception ports")
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return err
	}
	_, err := Mourn(p)
	return err
}

func requestManualStop(p *Process) (err error) {
	var (
		task          = C.mach_port_t(p.os.task)
		thread        = C.mach_port_t(p.CurrentThread.os.threadAct)
		exceptionPort = C.mach_port_t(p.os.exceptionPort)
	)
	kret := C.raise_exception(task, thread, exceptionPort, C.EXC_BREAKPOINT)
	if kret != C.KERN_SUCCESS {
		return fmt.Errorf("could not raise mach exception")
	}
	return nil
}

func (p *Process) updateThreadList() error {
	var (
		err   error
		kret  C.kern_return_t
		count C.int
		list  []uint32
	)

	for {
		count = C.thread_count(p.os.task)
		if count == -1 {
			return fmt.Errorf("could not get thread count")
		}
		list = make([]uint32, count)

		// TODO(dp) might be better to malloc mem in C and then free it here
		// instead of getting count above and passing in a slice
		kret = C.get_threads(p.os.task, unsafe.Pointer(&list[0]), count)
		if kret != -2 {
			break
		}
	}
	if kret != C.KERN_SUCCESS {
		return fmt.Errorf("could not get thread list")
	}

	for _, port := range list {
		if _, ok := p.Threads[int(port)]; !ok {
			_, err = p.addThread(int(port), false)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *Process) addThread(port int, attach bool) (*Thread, error) {
	if thread, ok := p.Threads[port]; ok {
		return thread, nil
	}
	thread := &Thread{
		ID: port,
		p:  p,
		os: new(OSSpecificDetails),
	}
	p.Threads[port] = thread
	thread.os.threadAct = C.thread_act_t(port)
	if p.CurrentThread == nil {
		p.SetActiveThread(thread)
	}
	return thread, nil
}

func findExecutable(pid int, path string) (string, error) {
	if path == "" {
		path = C.GoString(C.find_executable(C.int(pid)))
	}
	return path, nil
}

func mourn(p *Process) (int, error) {
	var status int
	for {
		log.Debug("begin mourn wait")
		_, ws, err := wait4(p.Pid, 0)
		log.Debug("fin mourn wait")
		if err != nil {
			if err != syscall.ECHILD {
				return 0, err
			}
			break
		}
		if ws != nil && (ws.Exited() || ws.Signal() == sys.SIGKILL) {
			status = ws.ExitStatus()
			break
		}
	}
	selfTask := C.ipc_space_t(C.mach_task())
	for _, th := range p.Threads {
		C.mach_port_deallocate(selfTask, C.mach_port_name_t(th.os.threadAct))
	}
	C.mach_port_destroy(selfTask, C.mach_port_name_t(p.os.notificationPort))
	C.mach_port_deallocate(selfTask, C.mach_port_name_t(p.os.exceptionPort))
	C.mach_port_deallocate(selfTask, C.mach_port_name_t(p.os.task))
	return status, nil
}

func wait(p *Process, pid int) (*WaitStatus, *Thread, error) {
	for {
		var hdr C.mach_msg_header_t
		var sig C.int
		port := C.mach_port_wait(p.os.portSet, &hdr, &sig, C.int(0))
		th, ok := p.Threads[int(port)]
		if ok {
			th.os.msgStop = true
			th.os.hdr = hdr
			th.os.sig = sig
		}
		switch port {
		case p.os.notificationPort:
			exitcode, err := Mourn(p)
			if err != nil {
				return nil, nil, err
			}
			return &WaitStatus{exited: true, exitstatus: exitcode}, nil, nil

		case C.MACH_RCV_INTERRUPTED:
			if !p.halt {
				// Call wait again, it seems
				// MACH_RCV_INTERRUPTED is emitted before
				// process natural death _sometimes_.
				continue
			}
			return &WaitStatus{signal: syscall.Signal(int(sig)), signaled: true}, nil, nil

		case 0:
			return nil, nil, fmt.Errorf("error while waiting for task")
		}

		// Since we cannot be notified of new threads on OS X
		// this is as good a time as any to check for them.
		p.updateThreadList()
		// for {
		// 	var hdr C.mach_msg_header_t
		// 	var nsig C.int
		// 	port := C.mach_port_wait(p.os.portSet, &hdr, &nsig, C.int(1))
		// 	if port == 0 {
		// 		break
		// 	}
		// 	if port == C.MACH_RCV_TIMED_OUT {
		// 		break
		// 	}
		// 	if port == p.os.notificationPort {
		// 		exitcode, err := Mourn(p)
		// 		if err != nil {
		// 			return nil, nil, err
		// 		}
		// 		return &WaitStatus{exited: true, exitstatus: exitcode}, nil, nil
		// 	}
		// 	if th, ok := p.Threads[int(port)]; ok {
		// 		th.os.msgStop = true
		// 		th.os.hdr = hdr
		// 		th.os.sig = nsig
		// 	}
		// }
		log.WithField("signal", int(sig)).Debug("wait finished")
		return &WaitStatus{signal: syscall.Signal(int(sig)), signaled: int(sig) != 0}, th, nil
	}
}

func (p *Process) loadProcessInformation() {
	return
}

func wait4(pid, options int) (int, *sys.WaitStatus, error) {
	_, f, l, _ := runtime.Caller(1)
	log.WithFields(logrus.Fields{"pid": pid, "caller": fmt.Sprintf("%s:%d", filepath.Base(f), l)}).Debug("wait called")
	var status sys.WaitStatus
	wpid, err := sys.Wait4(pid, &status, options, nil)
	return wpid, &status, err
}
