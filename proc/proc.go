package proc

import (
	"debug/dwarf"
	"debug/gosym"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	sys "golang.org/x/sys/unix"

	"github.com/derekparker/delve/dwarf/frame"
	"github.com/derekparker/delve/dwarf/line"
	"github.com/derekparker/delve/dwarf/reader"
	"github.com/derekparker/delve/source"
)

// Process represents all of the information the debugger
// is holding onto regarding the process we are debugging.
type Process struct {
	Pid     int         // Process Pid
	Process *os.Process // Pointer to process struct for the actual process we are debugging

	// Breakpoint table, hold information on software / hardware breakpoints.
	// Maps instruction address to Breakpoint struct.
	Breakpoints map[uint64]*Breakpoint

	// List of threads mapped as such: pid -> *Thread
	Threads map[int]*Thread

	// Active thread. This is the default thread used for setting breakpoints, evaluating variables, etc..
	CurrentThread *Thread

	dwarf                   *dwarf.Data
	goSymTable              *gosym.Table
	frameEntries            frame.FrameDescriptionEntries
	lineInfo                *line.DebugLineInfo
	firstStart              bool
	singleStepping          bool
	os                      *OSProcessDetails
	arch                    Arch
	ast                     *source.Searcher
	breakpointIDCounter     int
	tempBreakpointIDCounter int
	running                 bool
	halt                    bool
	exited                  bool
	ptraceChan              chan func()
	ptraceDoneChan          chan interface{}
}

func New(pid int) *Process {
	dbp := &Process{
		Pid:            pid,
		Threads:        make(map[int]*Thread),
		Breakpoints:    make(map[uint64]*Breakpoint),
		firstStart:     true,
		os:             new(OSProcessDetails),
		ast:            source.New(),
		ptraceChan:     make(chan func()),
		ptraceDoneChan: make(chan interface{}),
	}
	go dbp.handlePtraceFuncs()
	return dbp
}

// A ManualStopError happens when the user triggers a
// manual stop via SIGERM.
type ManualStopError struct{}

func (mse ManualStopError) Error() string {
	return "Manual stop requested"
}

// ProcessExitedError indicates that the process has exited and contains both
// process id and exit status.
type ProcessExitedError struct {
	Pid    int
	Status int
}

func (pe ProcessExitedError) Error() string {
	return fmt.Sprintf("process %d has exited with status %d", pe.Pid, pe.Status)
}

// Attach to an existing process with the given PID.
func Attach(pid int) (*Process, error) {
	dbp, err := initializeDebugProcess(New(pid), "", true)
	if err != nil {
		return nil, err
	}
	return dbp, nil
}

func (dbp *Process) Detach(kill bool) (err error) {
	// Clean up any breakpoints we've set.
	for _, bp := range dbp.Breakpoints {
		if bp != nil {
			_, err := dbp.ClearBreakpoint(bp.Addr)
			if err != nil {
				return err
			}
		}
	}
	dbp.execPtraceFunc(func() {
		var sig int
		if kill {
			sig = int(sys.SIGINT)
		}
		err = PtraceDetach(dbp.Pid, sig)
	})
	return
}

// Returns whether or not Delve thinks the debugged
// process has exited.
func (dbp *Process) Exited() bool {
	return dbp.exited
}

// Returns whether or not Delve thinks the debugged
// process is currently executing.
func (dbp *Process) Running() bool {
	return dbp.running
}

// Finds the executable and then uses it
// to parse the following information:
// * Dwarf .debug_frame section
// * Dwarf .debug_line section
// * Go symbol table.
func (dbp *Process) LoadInformation(path string) error {
	var wg sync.WaitGroup

	exe, err := dbp.findExecutable(path)
	if err != nil {
		return err
	}

	wg.Add(3)
	go dbp.parseDebugFrame(exe, &wg)
	go dbp.obtainGoSymbols(exe, &wg)
	go dbp.parseDebugLineInfo(exe, &wg)
	wg.Wait()

	return nil
}

// Find a location by string (file+line, function, breakpoint id, addr)
func (dbp *Process) FindLocation(str string) (uint64, error) {
	// File + Line
	if strings.ContainsRune(str, ':') {
		fl := strings.Split(str, ":")

		fileName, err := filepath.Abs(fl[0])
		if err != nil {
			return 0, err
		}

		line, err := strconv.Atoi(fl[1])
		if err != nil {
			return 0, err
		}

		pc, _, err := dbp.goSymTable.LineToPC(fileName, line)
		if err != nil {
			return 0, err
		}
		return pc, nil
	}

	// Try to lookup by function name
	fn := dbp.goSymTable.LookupFunc(str)
	if fn != nil {
		return fn.Entry, nil
	}

	// Attempt to parse as number for breakpoint id or raw address
	id, err := strconv.ParseUint(str, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("unable to find location for %s", str)
	}

	for _, bp := range dbp.Breakpoints {
		if uint64(bp.ID) == id {
			return bp.Addr, nil
		}
	}

	// Last resort, use as raw address
	return id, nil
}

// Sends out a request that the debugged process halt
// execution. Sends SIGSTOP to all threads.
func (dbp *Process) RequestManualStop() error {
	dbp.halt = true
	err := dbp.requestManualStop()
	if err != nil {
		return err
	}
	err = dbp.Halt()
	if err != nil {
		return err
	}
	dbp.running = false
	return nil
}

// Sets a breakpoint at addr, and stores it in the process wide
// break point table. Setting a break point must be thread specific due to
// ptrace actions needing the thread to be in a signal-delivery-stop.
//
// Depending on hardware support, Delve will choose to either
// set a hardware or software breakpoint. Essentially, if the
// hardware supports it, and there are free debug registers, Delve
// will set a hardware breakpoint. Otherwise we fall back to software
// breakpoints, which are a bit more work for us.
func (dbp *Process) SetBreakpoint(addr uint64) (*Breakpoint, error) {
	return dbp.setBreakpoint(dbp.CurrentThread.Id, addr, false)
}

// Sets a temp breakpoint, for the 'next' command.
func (dbp *Process) SetTempBreakpoint(addr uint64) (*Breakpoint, error) {
	return dbp.setBreakpoint(dbp.CurrentThread.Id, addr, true)
}

// Sets a breakpoint by location string (function, file+line, address)
func (dbp *Process) SetBreakpointByLocation(loc string) (*Breakpoint, error) {
	addr, err := dbp.FindLocation(loc)
	if err != nil {
		return nil, err
	}
	return dbp.SetBreakpoint(addr)
}

// Clears a breakpoint in the current thread.
func (dbp *Process) ClearBreakpoint(addr uint64) (*Breakpoint, error) {
	return dbp.clearBreakpoint(dbp.CurrentThread.Id, addr)
}

// Clears a breakpoint by location (function, file+line, address, breakpoint id)
func (dbp *Process) ClearBreakpointByLocation(loc string) (*Breakpoint, error) {
	addr, err := dbp.FindLocation(loc)
	if err != nil {
		return nil, err
	}
	return dbp.ClearBreakpoint(addr)
}

// Returns the status of the current main thread context.
func (dbp *Process) Status() *sys.WaitStatus {
	return dbp.CurrentThread.Status
}

// Step over function calls.
func (dbp *Process) Next() error {
	return dbp.run(dbp.next)
}

func (dbp *Process) next() error {
	// Make sure we clean up the temp breakpoints created by thread.Next
	defer dbp.clearTempBreakpoints()

	chanRecvCount, err := dbp.setChanRecvBreakpoints()
	if err != nil {
		return err
	}

	g, err := dbp.CurrentThread.getG()
	if err != nil {
		return err
	}

	if g.DeferPC != 0 {
		_, err = dbp.SetTempBreakpoint(g.DeferPC)
		if err != nil {
			return err
		}
	}

	var goroutineExiting bool
	var waitCount int
	for _, th := range dbp.Threads {
		if th.blocked() {
			// Ignore threads that aren't running go code.
			continue
		}
		waitCount++
		if err = th.SetNextBreakpoints(); err != nil {
			if err, ok := err.(GoroutineExitingError); ok {
				waitCount = waitCount - 1 + chanRecvCount
				if err.goid == g.Id {
					goroutineExiting = true
				}
				continue
			}
			return err
		}
	}
	for _, th := range dbp.Threads {
		if err = th.Continue(); err != nil {
			return err
		}
	}

	for waitCount > 0 {
		thread, err := dbp.trapWait(-1)
		if err != nil {
			return err
		}
		tg, err := thread.getG()
		if err != nil {
			return err
		}
		// Make sure we're on the same goroutine, unless it has exited.
		if tg.Id == g.Id || goroutineExiting {
			if dbp.CurrentThread != thread {
				dbp.SwitchThread(thread.Id)
			}
		}
		waitCount--
	}
	return dbp.Halt()
}

func (dbp *Process) setChanRecvBreakpoints() (int, error) {
	var count int
	allg, err := dbp.GoroutinesInfo()
	if err != nil {
		return 0, err
	}
	for _, g := range allg {
		if g.ChanRecvBlocked() {
			ret, err := g.chanRecvReturnAddr(dbp)
			if err != nil {
				if _, ok := err.(NullAddrError); ok {
					continue
				}
				return 0, err
			}
			if _, err = dbp.SetTempBreakpoint(ret); err != nil {
				return 0, err
			}
			count++
		}
	}
	return count, nil
}

// Resume process.
func (dbp *Process) Continue() error {
	for _, thread := range dbp.Threads {
		err := thread.Continue()
		if err != nil {
			return err
		}
	}
	return dbp.run(dbp.resume)
}

func (dbp *Process) resume() error {
	thread, err := dbp.trapWait(-1)
	if err != nil {
		return err
	}
	if dbp.CurrentThread != thread {
		dbp.SwitchThread(thread.Id)
	}
	pc, err := thread.PC()
	if err != nil {
		return err
	}
	if dbp.CurrentBreakpoint != nil || dbp.halt {
		return dbp.Halt()
	}
	// Check to see if we hit a runtime.breakpoint
	fn := dbp.goSymTable.PCToFunc(pc)
	if fn != nil && fn.Name == "runtime.breakpoint" {
		// step twice to get back to user code
		for i := 0; i < 2; i++ {
			if err = thread.Step(); err != nil {
				return err
			}
		}
		return dbp.Halt()
	}

	return fmt.Errorf("unrecognized breakpoint %#v", pc)
}

// Single step, will execute a single instruction.
func (dbp *Process) Step() (err error) {
	fn := func() error {
		dbp.singleStepping = true
		defer func() { dbp.singleStepping = false }()
		for _, th := range dbp.Threads {
			if th.blocked() {
				continue
			}
			err := th.Step()
			if err != nil {
				return err
			}
		}
		return nil
	}

	return dbp.run(fn)
}

// Change from current thread to the thread specified by `tid`.
func (dbp *Process) SwitchThread(tid int) error {
	if th, ok := dbp.Threads[tid]; ok {
		fmt.Printf("thread context changed from %d to %d\n", dbp.CurrentThread.Id, tid)
		dbp.CurrentThread = th
		return nil
	}
	return fmt.Errorf("thread %d does not exist", tid)
}

// Returns an array of G structures representing the information
// Delve cares about from the internal runtime G structure.
func (dbp *Process) GoroutinesInfo() ([]*G, error) {
	var (
		threadg = map[int]*Thread{}
		allg    []*G
		rdr     = dbp.DwarfReader()
	)

	for i := range dbp.Threads {
		if dbp.Threads[i].blocked() {
			continue
		}
		g, _ := dbp.Threads[i].getG()
		if g != nil {
			threadg[g.Id] = dbp.Threads[i]
		}
	}

	addr, err := rdr.AddrFor("runtime.allglen")
	if err != nil {
		return nil, err
	}
	allglenBytes, err := dbp.CurrentThread.readMemory(uintptr(addr), 8)
	if err != nil {
		return nil, err
	}
	allglen := binary.LittleEndian.Uint64(allglenBytes)

	rdr.Seek(0)
	allgentryaddr, err := rdr.AddrFor("runtime.allg")
	if err != nil {
		return nil, err
	}
	faddr, err := dbp.CurrentThread.readMemory(uintptr(allgentryaddr), dbp.arch.PtrSize())
	allgptr := binary.LittleEndian.Uint64(faddr)

	for i := uint64(0); i < allglen; i++ {
		g, err := parseG(dbp.CurrentThread, allgptr+(i*uint64(dbp.arch.PtrSize())), true)
		if err != nil {
			return nil, err
		}
		if thread, allocated := threadg[g.Id]; allocated {
			g.thread = thread
		}
		allg = append(allg, g)
	}
	return allg, nil
}

// Stop all threads.
func (dbp *Process) Halt() (err error) {
	for _, th := range dbp.Threads {
		if err := th.Halt(); err != nil {
			return err
		}
	}
	return nil
}

// Obtains register values from what Delve considers to be the current
// thread of the traced process.
func (dbp *Process) Registers() (Registers, error) {
	return dbp.CurrentThread.Registers()
}

// Returns the PC of the current thread.
func (dbp *Process) PC() (uint64, error) {
	return dbp.CurrentThread.PC()
}

// Returns the PC of the current thread.
func (dbp *Process) CurrentBreakpoint() *Breakpoint {
	return dbp.CurrentThread.CurrentBreakpoint
}

// Returns the value of the named symbol.
func (dbp *Process) EvalVariable(name string) (*Variable, error) {
	return dbp.CurrentThread.EvalVariable(name)
}

// Returns a reader for the dwarf data
func (dbp *Process) DwarfReader() *reader.Reader {
	return reader.New(dbp.dwarf)
}

// Returns list of source files that comprise the debugged binary.
func (dbp *Process) Sources() map[string]*gosym.Obj {
	return dbp.goSymTable.Files
}

// Returns list of functions present in the debugged program.
func (dbp *Process) Funcs() []gosym.Func {
	return dbp.goSymTable.Funcs
}

// Converts an instruction address to a file/line/function.
func (dbp *Process) PCToLine(pc uint64) (string, int, *gosym.Func) {
	return dbp.goSymTable.PCToLine(pc)
}

// Finds the breakpoint for the given ID.
func (dbp *Process) FindBreakpointByID(id int) (*Breakpoint, bool) {
	for _, bp := range dbp.Breakpoints {
		if bp.ID == id {
			return bp, true
		}
	}
	return nil, false
}

// Finds the breakpoint for the given pc.
func (dbp *Process) FindBreakpoint(pc uint64) (*Breakpoint, bool) {
	// Check for software breakpoint. PC will be at
	// breakpoint instruction + size of breakpoint.
	if bp, ok := dbp.Breakpoints[pc-uint64(dbp.arch.BreakpointSize())]; ok {
		return bp, true
	}
	// Check for hardware breakpoint. PC will equal
	// the breakpoint address since the CPU will stop
	// the process without executing the instruction at
	// this address.
	if bp, ok := dbp.Breakpoints[pc]; ok {
		return bp, true
	}
	return nil, false
}

// Returns a new Process struct.
func initializeDebugProcess(dbp *Process, path string, attach bool) (*Process, error) {
	if attach {
		var err error
		dbp.execPtraceFunc(func() { err = sys.PtraceAttach(dbp.Pid) })
		if err != nil {
			return nil, err
		}
		_, _, err = wait(dbp.Pid, 0)
		if err != nil {
			return nil, err
		}
	}

	proc, err := os.FindProcess(dbp.Pid)
	if err != nil {
		return nil, err
	}

	dbp.Process = proc
	err = dbp.LoadInformation(path)
	if err != nil {
		return nil, err
	}

	if err := dbp.updateThreadList(); err != nil {
		return nil, err
	}

	switch runtime.GOARCH {
	case "amd64":
		dbp.arch = AMD64Arch()
	}

	return dbp, nil
}

func (dbp *Process) clearTempBreakpoints() error {
	for _, bp := range dbp.Breakpoints {
		if !bp.Temp {
			continue
		}
		if _, err := dbp.ClearBreakpoint(bp.Addr); err != nil {
			return err
		}
	}
	return nil
}

func (dbp *Process) handleBreakpointOnThread(id int) (*Thread, error) {
	thread, ok := dbp.Threads[id]
	if !ok {
		return nil, fmt.Errorf("could not find thread for %d", id)
	}
	pc, err := thread.PC()
	if err != nil {
		return nil, err
	}
	// Check to see if we have hit a breakpoint.
	if bp, ok := dbp.FindBreakpoint(pc); ok {
		thread.CurrentBreakpoint = bp
		if err = thread.SetPC(bp.Addr); err != nil {
			return nil, err
		}
		return thread, nil
	}
	if dbp.halt {
		return thread, nil
	}
	fn := dbp.goSymTable.PCToFunc(pc)
	if fn != nil && fn.Name == "runtime.breakpoint" {
		thread.singleStepping = true
		defer func() { thread.singleStepping = false }()
		for i := 0; i < 2; i++ {
			if err := thread.Step(); err != nil {
				return nil, err
			}
		}
		return thread, nil
	}
	return nil, NoBreakpointError{addr: pc}
}

func (dbp *Process) run(fn func() error) error {
	if dbp.exited {
		return fmt.Errorf("process has already exited")
	}
	dbp.running = true
	dbp.halt = false
	for _, th := range dbp.Threads {
		th.CurrentBreakpoint = nil
	}
	defer func() { dbp.running = false }()
	if err := fn(); err != nil {
		if _, ok := err.(ManualStopError); !ok {
			return err
		}
	}
	return nil
}

func (dbp *Process) handlePtraceFuncs() {
	// We must ensure here that we are running on the same thread during
	// the execution of dbg. This is due to the fact that ptrace(2) expects
	// all commands after PTRACE_ATTACH to come from the same thread.
	runtime.LockOSThread()

	for fn := range dbp.ptraceChan {
		fn()
		dbp.ptraceDoneChan <- nil
	}
}

func (dbp *Process) execPtraceFunc(fn func()) {
	dbp.ptraceChan <- fn
	<-dbp.ptraceDoneChan
}
